package main

import (
	"bytes"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"text/template"

	"github.com/fsnotify/fsnotify"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"gopkg.in/yaml.v3"
)

var secretRegex = regexp.MustCompile(`(?i)(?:^|\s)(--password|-p|password=|secret=|token=)\s*(\S+)`)

type CommandData struct {
	*ServiceConfig
	Env map[string]string
}

type AppConfig struct {
	mu            sync.RWMutex
	Workdir       string `yaml:"workdir"`
	IdleTime      int    `yaml:"idletime"`
	Cooldown      int    `yaml:"cooldown"`
	MetricsPort   string `yaml:"metrics_port"`
	CheckInterval int    `yaml:"check_interval"`
	RefVal        string `yaml:"ref_val"`
	StartCMD      string `yaml:"start_cmd"`
	WatchCMD      string `yaml:"watch_cmd"`
	ShutdownCMD   string `yaml:"shutdown_cmd"`
}

type ServiceConfig struct {
	Name          string
	ContainerName string
	ListenPort    string
	TargetPort    string
	Path          string

	StartCMD      *string
	WatchCMD      *string
	ShutdownCMD   *string
	IdleTime      *int
	CheckInterval *int
	Cooldown      *int
	RefVal        *string
}

type Compose struct {
	Services map[string]struct {
		ContainerName string   `yaml:"container_name"`
		ListenPort    string   `yaml:"x-dip-listen_port"`
		StartCMD      *string  `yaml:"x-dip-start_cmd"`
		WatchCMD      *string  `yaml:"x-dip-watch_cmd"`
		ShutdownCMD   *string  `yaml:"x-dip-shutdown_cmd"`
		IdleTime      *int     `yaml:"x-dip-idletime"`
		CheckInterval *int     `yaml:"x-dip-check_interval"`
		Cooldown      *int     `yaml:"x-dip-cooldown"`
		RefVal        *string  `yaml:"x-dip-ref_val"`
		Ports         []string `yaml:"ports"`
	} `yaml:"services"`
}

const Version = "v1.0 (finalRelease)"

func init() {
	CustomRegistry.MustRegister(metricActiveConns)
	CustomRegistry.MustRegister(metricServiceRunning)
	CustomRegistry.MustRegister(metricStartsTotal)
	CustomRegistry.MustRegister(metricShutdownsTotal)
}

func SanitizeLog(input string) string {
	return secretRegex.ReplaceAllString(input, "${1} ***MASKED***")
}

func loadDotEnv(dirPath string) map[string]string {
	envMap := make(map[string]string)
	dotEnvPath := filepath.Join(dirPath, ".env")

	data, err := os.ReadFile(dotEnvPath)
	if err != nil {
		return envMap
	}

	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		pair := strings.SplitN(line, "=", 2)
		if len(pair) == 2 {
			key := strings.TrimSpace(pair[0])
			val := strings.TrimSpace(pair[1])
			val = strings.Trim(val, `"'`)
			envMap[key] = val
		}
	}
	return envMap
}

func getEnvMap() map[string]string {
	envMap := make(map[string]string)
	for _, e := range os.Environ() {
		pair := strings.SplitN(e, "=", 2)
		if len(pair) == 2 {
			envMap[pair[0]] = pair[1]
		}
	}
	return envMap
}

func LoadConfig(path string) (*AppConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var cfg AppConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}

	if cfg.Cooldown <= 0 {
		cfg.Cooldown = cfg.IdleTime
	}
	if cfg.MetricsPort == "" {
		cfg.MetricsPort = ":9090"
	}
	if !strings.HasPrefix(cfg.MetricsPort, ":") {
		cfg.MetricsPort = ":" + cfg.MetricsPort
	}

	return &cfg, nil
}

func (c *AppConfig) UpdateFromFile(path string) error {
	newCfg, err := LoadConfig(path)
	if err != nil {
		return err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	c.Workdir = newCfg.Workdir
	c.IdleTime = newCfg.IdleTime
	c.Cooldown = newCfg.Cooldown
	c.MetricsPort = newCfg.MetricsPort
	c.CheckInterval = newCfg.CheckInterval
	c.RefVal = newCfg.RefVal
	c.StartCMD = newCfg.StartCMD
	c.WatchCMD = newCfg.WatchCMD
	c.ShutdownCMD = newCfg.ShutdownCMD
	return nil
}

func resolveConfigPath(flagPath string) (string, error) {
	if flagPath != "" {
		fi, err := os.Stat(flagPath)
		if err != nil {
			return "", fmt.Errorf("specified config path is unreachable: %w", err)
		}
		if fi.IsDir() {
			if _, err := os.Stat(filepath.Join(flagPath, "config.yaml")); err == nil {
				return filepath.Join(flagPath, "config.yaml"), nil
			}
			if _, err := os.Stat(filepath.Join(flagPath, "config.yml")); err == nil {
				return filepath.Join(flagPath, "config.yml"), nil
			}
			return "", fmt.Errorf("no config.yaml or config.yml found in directory: %s", flagPath)
		}
		return flagPath, nil
	}

	defaultDir := "/etc/docker-idle-proxy"
	if _, err := os.Stat(filepath.Join(defaultDir, "config.yaml")); err == nil {
		return filepath.Join(defaultDir, "config.yaml"), nil
	}
	if _, err := os.Stat(filepath.Join(defaultDir, "config.yml")); err == nil {
		return filepath.Join(defaultDir, "config.yml"), nil
	}

	if _, err := os.Stat("config.yaml"); err == nil {
		return "config.yaml", nil
	}

	return "", fmt.Errorf("no configuration file found")
}

func RenderCommand(cmdTpl string, srv *ServiceConfig) (string, error) {
	tmpl, err := template.New("cmd").Option("missingkey=zero").Parse(cmdTpl)
	if err != nil {
		return "", err
	}

	envMap := getEnvMap()
	localEnv := loadDotEnv(srv.Path)
	for k, v := range localEnv {
		envMap[k] = v
	}

	data := CommandData{
		ServiceConfig: srv,
		Env:           envMap,
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", err
	}

	return buf.String(), nil
}

func ParseCompose(path string) (*ServiceConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var comp Compose
	if err := yaml.Unmarshal(data, &comp); err != nil {
		return nil, err
	}

	var node yaml.Node
	if err := yaml.Unmarshal(data, &node); err != nil {
		return nil, err
	}

	firstServiceName := getFirstServiceName(&node)
	if firstServiceName == "" {
		return nil, fmt.Errorf("no services found in compose file: %s", path)
	}

	srv, exists := comp.Services[firstServiceName]
	if !exists {
		return nil, fmt.Errorf("failed to locate first service '%s' in structure: %s", firstServiceName, path)
	}

	if len(srv.Ports) == 0 {
		return nil, fmt.Errorf("first service '%s' has no ports defined: %s", firstServiceName, path)
	}

	parts := strings.Split(srv.Ports[0], ":")
	targetPort := ""

	if len(parts) == 2 {
		targetPort = parts[0]
	} else if len(parts) >= 3 {
		targetPort = parts[len(parts)-2]
	}

	listenPort := srv.ListenPort
	if listenPort == "" {
		listenPort = targetPort
	}

	containerName := srv.ContainerName
	if containerName == "" {
		containerName = firstServiceName
	}

	return &ServiceConfig{
		Name:          firstServiceName,
		ContainerName: containerName,
		ListenPort:    listenPort,
		TargetPort:    targetPort,
		Path:          filepath.Dir(path),
		StartCMD:      srv.StartCMD,
		WatchCMD:      srv.WatchCMD,
		ShutdownCMD:   srv.ShutdownCMD,
		IdleTime:      srv.IdleTime,
		CheckInterval: srv.CheckInterval,
		Cooldown:      srv.Cooldown,
		RefVal:        srv.RefVal,
	}, nil
}

func getFirstServiceName(node *yaml.Node) string {
	if node.Kind != yaml.DocumentNode || len(node.Content) == 0 {
		return ""
	}
	root := node.Content[0]
	if root.Kind != yaml.MappingNode {
		return ""
	}

	for i := 0; i < len(root.Content); i += 2 {
		keyNode := root.Content[i]
		valNode := root.Content[i+1]

		if keyNode.Value == "services" && valNode.Kind == yaml.MappingNode {
			if len(valNode.Content) >= 2 {
				return valNode.Content[0].Value
			}
		}
	}
	return ""
}

func WatchWorkdir(workdir string, configPath string, cfg *AppConfig, proxies map[string]*ServiceProxy, mu *sync.Mutex) error {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer watcher.Close()

	_ = watcher.Add(configPath)

	err = filepath.Walk(workdir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			_ = watcher.Add(path)
		} else if isComposeFile(info.Name()) {
			if srv, err := ParseCompose(path); err == nil {
				mu.Lock()
				if _, exists := proxies[srv.Path]; !exists {
					p := NewServiceProxy(srv, cfg)
					proxies[srv.Path] = p
					go p.StartListening()
				}
				mu.Unlock()
			}
		}
		return nil
	})
	if err != nil {
		return err
	}

	log.Printf("Folder & Config monitoring started. Workdir: %s", workdir)

	for {
		select {
		case event, ok := <-watcher.Events:
			if !ok {
				return nil
			}

			if event.Has(fsnotify.Write) || event.Has(fsnotify.Create) {
				if filepath.Clean(event.Name) == filepath.Clean(configPath) {
					log.Printf("[CONFIG] Global config changed! Reloading %s...", configPath)
					if err := cfg.UpdateFromFile(configPath); err != nil {
						log.Printf("[CONFIG] Failed to reload config: %v", err)
					} else {
						log.Printf("[CONFIG] Global config successfully reloaded.")
					}
					continue
				}

				fi, err := os.Stat(event.Name)
				if err == nil && fi.IsDir() {
					_ = watcher.Add(event.Name)
					continue
				}

				if isComposeOrEnvFile(event.Name) {
					dir := filepath.Dir(event.Name)
					composePath := findComposePath(dir)
					if composePath == "" {
						continue
					}

					mu.Lock()
					proxy, exists := proxies[dir]
					mu.Unlock()

					if exists {
						log.Printf("[RELOAD] File changed (%s). Updating stack...", filepath.Base(event.Name))
						go proxy.ReloadAndRestart(composePath)
					} else {
						if srv, err := ParseCompose(composePath); err == nil {
							mu.Lock()
							p := NewServiceProxy(srv, cfg)
							proxies[srv.Path] = p
							go p.StartListening()
							mu.Unlock()
						}
					}
				}
			}

			if event.Has(fsnotify.Remove) || event.Has(fsnotify.Rename) {
				dir := event.Name
				if !isComposeOrEnvFile(event.Name) {
					dir = filepath.Clean(event.Name)
				} else {
					dir = filepath.Dir(event.Name)
				}

				mu.Lock()
				proxy, exists := proxies[dir]
				if exists {
					log.Printf("[DELETE] Stack/Directory removed (%s). Stopping proxy and container...", dir)
					delete(proxies, dir)
					mu.Unlock()

					go proxy.Stop()
				} else {
					mu.Unlock()
				}
			}

		case err, ok := <-watcher.Errors:
			if !ok {
				return nil
			}
			log.Println("fsnotify error:", err)
		}
	}
}

func isComposeFile(name string) bool {
	base := filepath.Base(name)
	return base == "docker-compose.yml" || base == "docker-compose.yaml"
}

func isComposeOrEnvFile(name string) bool {
	base := filepath.Base(name)
	return base == "docker-compose.yml" || base == "docker-compose.yaml" || base == ".env"
}

func findComposePath(dir string) string {
	if _, err := os.Stat(filepath.Join(dir, "docker-compose.yml")); err == nil {
		return filepath.Join(dir, "docker-compose.yml")
	}
	if _, err := os.Stat(filepath.Join(dir, "docker-compose.yaml")); err == nil {
		return filepath.Join(dir, "docker-compose.yaml")
	}
	return ""
}

func main() {
        var (
		configFlag  string
		versionFlag bool
		helpFlag    bool
	)

	// Flag definíciók (rövid és hosszú opciók ugyanarra a változóra mutathatnak, vagy külön kötve)
	flag.StringVar(&configFlag, "c", "", "Path to configuration file or directory")
	flag.StringVar(&configFlag, "config", "", "Path to configuration file or directory")

	flag.BoolVar(&versionFlag, "v", false, "Print version and exit")
	flag.BoolVar(&versionFlag, "version", false, "Print version and exit")

	flag.BoolVar(&helpFlag, "h", false, "Print this help")
	flag.BoolVar(&helpFlag, "help", false, "Print this help")

	// Egyedi --help / -h kimenet megformázása
	flag.Usage = func() {
		fmt.Println("Usage of ./docker-idle-proxy:")
		fmt.Println("  -h, --help")
		fmt.Println("        Print this help")
		fmt.Println("  -c, --config string")
		fmt.Println("        Path to configuration file or directory")
		fmt.Println("  -v, --version")
		fmt.Println("        Print version and exit")
	}

	flag.Parse()

	if helpFlag {
		flag.Usage()
		os.Exit(0)
	}

	if versionFlag {
		fmt.Printf("docker-idle-proxy %s\n", Version)
		os.Exit(0)
	}

	configPath, err := resolveConfigPath(configFlag)
	if err != nil {
		log.Fatalf("Configuration error: %v", err)
	}

	cfg, err := LoadConfig(configPath)
	if err != nil {
		log.Fatalf("Error reading config: %v", err)
	}

	log.Printf("Config loaded from %s. Workdir: %s, MetricsPort: %s", configPath, cfg.Workdir, cfg.MetricsPort)

	http.Handle("/metrics", promhttp.HandlerFor(CustomRegistry, promhttp.HandlerOpts{}))
	go func() {
		log.Printf("Prometheus metrics server started: http://0.0.0.0%s/metrics", cfg.MetricsPort)
		if err := http.ListenAndServe(cfg.MetricsPort, nil); err != nil {
			log.Printf("Error running Prometheus HTTP server: %v", err)
		}
	}()

	proxies := make(map[string]*ServiceProxy)
	var mu sync.Mutex

	if err := WatchWorkdir(cfg.Workdir, configPath, cfg, proxies, &mu); err != nil {
		log.Fatalf("Error watching directory: %v", err)
	}
}



/home/csadi/dev/go/docker-idle-proxy/main.go
