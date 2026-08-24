package main

import (
	"fmt"
	"io"
	"log"
	"net"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

var CustomRegistry = prometheus.NewRegistry()

var (
	metricActiveConns = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "idle_proxy_active_connections",
		Help: "Current number of active TCP connections per service.",
	}, []string{"service"})

	metricServiceRunning = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "idle_proxy_service_running",
		Help: "State of the container/service (1 = running, 0 = stopped).",
	}, []string{"service"})

	metricStartsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "idle_proxy_starts_total",
		Help: "Total number of times the stack was started by the proxy.",
	}, []string{"service"})

	metricShutdownsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "idle_proxy_shutdowns_total",
		Help: "Total number of times the stack was stopped by the proxy.",
	}, []string{"service"})
)

type ServiceProxy struct {
	Config      *ServiceConfig
	AppCfg      *AppConfig
	activeConns int32
	mu          sync.RWMutex
	reloadMu    sync.Mutex
	lastActive  time.Time
	lastStarted time.Time
	isRunning   bool
	isStarting  bool
	listener    net.Listener
	stopChan    chan struct{}
}

func NewServiceProxy(srv *ServiceConfig, appCfg *AppConfig) *ServiceProxy {
	now := time.Now()
	sp := &ServiceProxy{
		Config:      srv,
		AppCfg:      appCfg,
		lastActive:  now,
		lastStarted: now,
		stopChan:    make(chan struct{}),
	}

	sp.isRunning = sp.checkContainerRunning()

	if sp.isRunning {
		log.Printf("[%s] Existing running container detected on startup.", srv.Name)
		metricServiceRunning.WithLabelValues(srv.Name).Set(1)
	} else {
		metricServiceRunning.WithLabelValues(srv.Name).Set(0)
	}
	metricActiveConns.WithLabelValues(srv.Name).Set(0)

	go sp.startIdleTicker()
	return sp
}

func (p *ServiceProxy) getStartCMD() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.Config.StartCMD != nil && *p.Config.StartCMD != "" {
		return *p.Config.StartCMD
	}
	p.AppCfg.mu.RLock()
	defer p.AppCfg.mu.RUnlock()
	return p.AppCfg.StartCMD
}

func (p *ServiceProxy) getWatchCMD() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.Config.WatchCMD != nil && *p.Config.WatchCMD != "" {
		return *p.Config.WatchCMD
	}
	p.AppCfg.mu.RLock()
	defer p.AppCfg.mu.RUnlock()
	return p.AppCfg.WatchCMD
}

func (p *ServiceProxy) getShutdownCMD() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.Config.ShutdownCMD != nil && *p.Config.ShutdownCMD != "" {
		return *p.Config.ShutdownCMD
	}
	p.AppCfg.mu.RLock()
	defer p.AppCfg.mu.RUnlock()
	return p.AppCfg.ShutdownCMD
}

func (p *ServiceProxy) getIdleTime() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.Config.IdleTime != nil {
		return *p.Config.IdleTime
	}
	p.AppCfg.mu.RLock()
	defer p.AppCfg.mu.RUnlock()
	return p.AppCfg.IdleTime
}

func (p *ServiceProxy) getCooldown() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.Config.Cooldown != nil {
		return *p.Config.Cooldown
	}
	p.AppCfg.mu.RLock()
	defer p.AppCfg.mu.RUnlock()
	return p.AppCfg.Cooldown
}

func (p *ServiceProxy) getCheckInterval() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.Config.CheckInterval != nil {
		return *p.Config.CheckInterval
	}
	p.AppCfg.mu.RLock()
	defer p.AppCfg.mu.RUnlock()
	return p.AppCfg.CheckInterval
}

func (p *ServiceProxy) getRefVal() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.Config.RefVal != nil {
		return *p.Config.RefVal
	}
	p.AppCfg.mu.RLock()
	defer p.AppCfg.mu.RUnlock()
	return p.AppCfg.RefVal
}

func (p *ServiceProxy) ReloadAndRestart(composePath string) {
	if !p.reloadMu.TryLock() {
		return
	}
	defer p.reloadMu.Unlock()

	newSrv, err := ParseCompose(composePath)
	if err != nil {
		p.mu.RLock()
		srvName := p.Config.Name
		p.mu.RUnlock()
		log.Printf("[%s] Error re-parsing compose during reload: %v", srvName, err)
		return
	}

	p.mu.Lock()
	oldPort := p.Config.ListenPort
	wasRunning := p.isRunning
	srvName := p.Config.Name
	p.Config = newSrv
	newPort := p.Config.ListenPort
	p.mu.Unlock()

	if oldPort != newPort {
		log.Printf("[%s] Port changed from :%s to :%s. Restarting listener...", srvName, oldPort, newPort)
		p.mu.Lock()
		if p.listener != nil {
			l := p.listener
			p.listener = nil
			l.Close()
		}
		p.mu.Unlock()

		go p.StartListening()
	}

	if !wasRunning {
		log.Printf("[%s] Compose updated while container is stopped. Config reloaded in memory.", srvName)
		return
	}

	p.mu.Lock()
	p.isRunning = false
	p.mu.Unlock()

	log.Printf("[%s] Reloading running service: Executing shutdownCMD...", srvName)
	if err := p.shutdownContainer(); err != nil {
		log.Printf("[%s] Reloading aborted: Shutdown failed (%v). Leaving current state untouched.", srvName, err)
		return
	}

	log.Printf("[%s] Reloading running service: Executing startCMD...", srvName)
	if err := p.startContainer(); err != nil {
		log.Printf("[%s] Reloading: Start error: %v", srvName, err)
		return
	}

	p.mu.Lock()
	p.isRunning = true
	p.lastStarted = time.Now()
	metricServiceRunning.WithLabelValues(srvName).Set(1)
	metricStartsTotal.WithLabelValues(srvName).Inc()
	p.mu.Unlock()

	log.Printf("[%s] Stack restarted successfully after compose update.", srvName)
}

func (p *ServiceProxy) StartListening() {
	p.mu.RLock()
	listenPort := p.Config.ListenPort
	srvName := p.Config.Name
	targetPort := p.Config.TargetPort
	p.mu.RUnlock()

	addr := ":" + listenPort
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		log.Printf("[%s] Failed to open port (%s): %v", srvName, addr, err)
		return
	}

	p.mu.Lock()
	p.listener = listener
	p.mu.Unlock()

	defer listener.Close()

	log.Printf("[%s] Proxy active: Public port :%s -> Internal target port 127.0.0.1:%s",
		srvName, listenPort, targetPort)

	for {
		conn, err := listener.Accept()
		if err != nil {
			p.mu.RLock()
			isClosed := p.listener == nil
			p.mu.RUnlock()
			if isClosed {
				return
			}
			log.Printf("[%s] Connection error: %v", srvName, err)
			continue
		}

		go p.handleConnection(conn)
	}
}

func (p *ServiceProxy) Stop() {
	p.mu.Lock()
	select {
	case <-p.stopChan:
	default:
		close(p.stopChan)
	}
	if p.listener != nil {
		l := p.listener
		p.listener = nil
		l.Close()
	}
	p.mu.Unlock()
	p.shutdownContainer()
}

func (p *ServiceProxy) handleConnection(clientConn net.Conn) {
	p.mu.Lock()
	p.activeConns++
	p.lastActive = time.Now()
	srvName := p.Config.Name
	targetPort := p.Config.TargetPort
	metricActiveConns.WithLabelValues(srvName).Set(float64(p.activeConns))
	p.mu.Unlock()

	defer func() {
		clientConn.Close()
		p.mu.Lock()
		p.activeConns--
		p.lastActive = time.Now()
		metricActiveConns.WithLabelValues(srvName).Set(float64(p.activeConns))
		p.mu.Unlock()
	}()

	if err := p.ensureContainerRunning(); err != nil {
		log.Printf("[%s] Failed to start service for client: %v", srvName, err)
		return
	}

	targetAddr := "127.0.0.1:" + targetPort
	var targetConn net.Conn
	var err error

	for i := 0; i < 45; i++ {
		targetConn, err = net.DialTimeout("tcp", targetAddr, 1*time.Second)
		if err == nil {
			break
		}
		time.Sleep(1 * time.Second)
	}

	if err != nil {
		log.Printf("[%s] Target port (%s) unavailable: %v", srvName, targetAddr, err)
		return
	}
	defer targetConn.Close()

	var wg sync.WaitGroup
	wg.Add(2)

	copyBuffer := func(dst net.Conn, src net.Conn) {
		defer wg.Done()
		_, _ = io.Copy(dst, src)
		_ = dst.Close()
		_ = src.Close()
	}

	go copyBuffer(targetConn, clientConn)
	go copyBuffer(clientConn, targetConn)

	wg.Wait()
}

func (p *ServiceProxy) ensureContainerRunning() error {
	p.mu.Lock()

	if p.isRunning {
		p.mu.Unlock()
		if !p.checkContainerRunning() {
			p.mu.Lock()
			log.Printf("[%s] Container was stopped externally! Updating state.", p.Config.Name)
			p.isRunning = false
			metricServiceRunning.WithLabelValues(p.Config.Name).Set(0)
			p.mu.Unlock()
		} else {
			return nil
		}
		p.mu.Lock()
	}

	for p.isStarting {
		p.mu.Unlock()
		time.Sleep(200 * time.Millisecond)
		p.mu.Lock()
		if p.isRunning {
			p.mu.Unlock()
			return nil
		}
	}

	p.isStarting = true
	srvName := p.Config.Name
	p.mu.Unlock()

	log.Printf("[%s] Incoming connection trigger! Starting container stack...", srvName)

	err := p.startContainer()

	p.mu.Lock()
	p.isStarting = false
	if err == nil {
		p.isRunning = true
		p.lastStarted = time.Now()
		metricServiceRunning.WithLabelValues(srvName).Set(1)
		metricStartsTotal.WithLabelValues(srvName).Inc()
	}
	p.mu.Unlock()

	return err
}

func (p *ServiceProxy) checkContainerRunning() bool {
	p.mu.RLock()
	containerName := p.Config.ContainerName
	p.mu.RUnlock()

	cmd := exec.Command("docker", "inspect", "-f", "{{.State.Running}}", containerName)
	out, err := cmd.Output()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) == "true"
}

func (p *ServiceProxy) startContainer() error {
	p.mu.RLock()
	srv := p.Config
	p.mu.RUnlock()

	tpl := p.getStartCMD()
	startCmdStr, err := RenderCommand(tpl, srv)
	if err != nil {
		return fmt.Errorf("startCMD render error: %w", err)
	}

	log.Printf("[%s] Executing StartCMD: %s", srv.Name, SanitizeLog(startCmdStr))

	cmd := exec.Command("sh", "-c", startCmdStr)
	out, err := cmd.CombinedOutput()
	if err != nil {
		log.Printf("[%s] Error executing start command:\n%s", srv.Name, SanitizeLog(string(out)))
		return err
	}
	return nil
}

func (p *ServiceProxy) shutdownContainer() error {
	p.mu.RLock()
	srv := p.Config
	p.mu.RUnlock()

	tpl := p.getShutdownCMD()
	shutdownCmdStr, err := RenderCommand(tpl, srv)
	if err != nil {
		log.Printf("[%s] ShutdownCMD render error: %v", srv.Name, err)
		return fmt.Errorf("shutdownCMD render error: %w", err)
	}

	log.Printf("[%s] Executing ShutdownCMD: %s", srv.Name, SanitizeLog(shutdownCmdStr))

	cmd := exec.Command("sh", "-c", shutdownCmdStr)
	out, err := cmd.CombinedOutput()
	if err != nil {
		log.Printf("[%s] Error executing shutdown command: %v | Output: %s", srv.Name, err, SanitizeLog(string(out)))
		return fmt.Errorf("shutdown execution failed: %w", err)
	}

	log.Printf("[%s] Container and Compose stack stopped successfully.", srv.Name)

	p.mu.Lock()
	p.isRunning = false
	metricServiceRunning.WithLabelValues(srv.Name).Set(0)
	metricShutdownsTotal.WithLabelValues(srv.Name).Inc()
	p.mu.Unlock()

	return nil
}

func (p *ServiceProxy) startIdleTicker() {
	currentInterval := p.getCheckInterval()
	if currentInterval <= 0 {
		currentInterval = 15
	}

	ticker := time.NewTicker(time.Duration(currentInterval) * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-p.stopChan:
			log.Printf("[%s] Idle ticker gracefully stopped.", p.Config.Name)
			return

		case <-ticker.C:
			newInterval := p.getCheckInterval()
			if newInterval <= 0 {
				newInterval = 15
			}
			if newInterval != currentInterval {
				currentInterval = newInterval
				ticker.Reset(time.Duration(currentInterval) * time.Second)
			}

			p.mu.RLock()
			active := p.activeConns
			last := p.lastActive
			started := p.lastStarted
			running := p.isRunning
			srv := p.Config
			p.mu.RUnlock()

			if !running {
				continue
			}

			if !p.checkContainerRunning() {
				log.Printf("[%s] Container stopped externally. Syncing internal state to stopped.", srv.Name)
				p.mu.Lock()
				p.isRunning = false
				metricServiceRunning.WithLabelValues(srv.Name).Set(0)
				p.mu.Unlock()
				continue
			}

			if active > 0 {
				log.Printf("[%s] Container running | Active TCP connections: %d", srv.Name, active)
				continue
			}

			cooldown := p.getCooldown()
			timeSinceStart := time.Since(started)
			if timeSinceStart < time.Duration(cooldown)*time.Second {
				log.Printf("[%s] Container running | Cooldown active (%v / %ds)",
					srv.Name, timeSinceStart.Round(time.Second), cooldown)
				continue
			}

			idleTime := p.getIdleTime()
			idleDuration := time.Since(last)
			log.Printf("[%s] Container running | Active TCP: 0 | Idle time: %v / %ds",
				srv.Name, idleDuration.Round(time.Second), idleTime)

			if idleDuration > time.Duration(idleTime)*time.Second {
				watchTpl := p.getWatchCMD()
				watchCmdStr, err := RenderCommand(watchTpl, srv)
				if err != nil {
					log.Printf("[%s] WatchCMD render error: %v", srv.Name, err)
					continue
				}

				out, err := exec.Command("sh", "-c", watchCmdStr).CombinedOutput()
				userCountStr := strings.TrimSpace(string(out))
				if err != nil {
					log.Printf("[%s] Error executing watchCMD: %v | Output: %s", srv.Name, err, SanitizeLog(userCountStr))
					continue
				}

				if userCountStr == "" {
					log.Printf("[%s] WARNING: watchCMD returned empty output! Check RCON password / command syntax.", srv.Name)
					continue
				}

				refVal := p.getRefVal()
				log.Printf("[%s] watchCMD check | Internal users: '%s' (Target for shutdown: '%s')",
					srv.Name, userCountStr, refVal)

				if userCountStr == refVal {
					log.Printf("[%s] Idle timeout reached (%ds) with '%s' active users. Initiating shutdown...",
						srv.Name, idleTime, userCountStr)

					p.shutdownContainer()
				}
			}
		}
	}
}
