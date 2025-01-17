package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/go-faster/sdk/app"
	"github.com/go-faster/sdk/zctx"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

type App struct {
	addr                       string
	node                       atomic.Pointer[[]byte]
	cfg                        atomic.Pointer[config]
	nodeExporterAddr           string
	agentAddr                  string
	targetsCount               int
	scrapeInterval             time.Duration
	scrapeConfigUpdateInterval time.Duration
	scrapeConfigUpdatePercent  float64
	useVictoria                bool
	targets                    []string
}

func (a *App) PollNodeExporter(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := a.fetchNodeExporter(ctx); err != nil {
				zctx.From(ctx).Error("cannot fetch node exporter", zap.Error(err))
			}
		}
	}
}

func (a *App) fetchNodeExporter(ctx context.Context) error {
	u := &url.URL{
		Scheme: "http",
		Path:   "/metrics",
		Host:   a.nodeExporterAddr,
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), http.NoBody)
	if err != nil {
		return err
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() {
		_ = res.Body.Close()
	}()
	data, err := io.ReadAll(res.Body)
	if err != nil {
		return err
	}
	a.node.Store(&data)

	// Count metrics.
	var metricsCount int
	s := bufio.NewScanner(bytes.NewReader(data))
	for s.Scan() {
		text := strings.TrimSpace(s.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		metricsCount++
	}

	d := sha256.Sum256(data)
	h := fmt.Sprintf("%x", d[:8])
	zctx.From(ctx).Info("fetched node exporter",
		zap.String("h", h),
		zap.Int("size", len(data)),
		zap.Int("metrics", metricsCount),
	)
	return nil
}

func (a *App) HandleConfig(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	v := a.cfg.Load()
	_, _ = w.Write(v.marshalYAML())
}

func (a *App) ProgressConfig(ctx context.Context) error {
	rev := 0
	r := rand.New(rand.NewSource(1)) // #nosec G404
	p := a.scrapeConfigUpdatePercent / 100
	ticker := time.NewTicker(a.scrapeConfigUpdateInterval)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			rev++
			revStr := fmt.Sprintf("r%d", rev)
			cfg := a.cfg.Load()
			for _, sc := range cfg.ScrapeConfigs {
				for _, stc := range sc.StaticConfigs {
					if r.Float64() >= p {
						continue
					}
					stc.Labels["revision"] = revStr
				}
			}
			a.cfg.Store(cfg)
		}
	}
}

func (a *App) HandleNodeExporter(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	v := a.node.Load()
	_, _ = w.Write(*v)
}

func (a *App) RunNodeExporter(ctx context.Context) error {
	args := []string{
		"--no-collector.wifi",
		"--no-collector.hwmon",
		"--no-collector.time",
		"--no-collector.timex",
		"--no-collector.arp",
		"--no-collector.netdev",
		"--no-collector.netstat",
		"--collector.processes",
		"--web.max-requests=40",
		"--web.listen-address=" + a.nodeExporterAddr,
		"--log.format=json",
	}
	cmd := exec.CommandContext(ctx, "node_exporter", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func (a *App) RunAgent(ctx context.Context) error {
	/*
	   - --httpListenAddr=:8429
	   - --remoteWrite.showURL
	   - --promscrape.config=http://0.0.0.0:8436/api/v1/config
	   - --promscrape.configCheckInterval={{ $.Values.scrapeConfigUpdateInterval }}
	   {{- range $urlReplica := until ($rs.writeURLReplicas | default 1 | int) }}
	   - --remoteWrite.url={{ $rs.writeURL }}?extra_label=url_replica={{ $urlReplica }}
	   {{- end }}
	   - --remoteWrite.tmpDataPath=/vmagent-data
	   - --remoteWrite.maxDiskUsagePerURL=100MiB
	   {{- if $.Values.writeConcurrency }}
	   - --remoteWrite.queues={{ $.Values.writeConcurrency }}
	   {{- end }}
	   - --remoteWrite.label=replica={{ $replica }}
	   - --promscrape.disableCompression
	   - --promscrape.noStaleMarkers
	   {{- if $rs.writeBearerToken }}
	   - --remoteWrite.bearerToken={{ $rs.writeBearerToken }}
	   {{- end }}
	   {{- if $rs.writeHeaders }}
	   - --remoteWrite.headers={{ $rs.writeHeaders }}
	   {{- end }}
	   {{- range $rs.vmagentExtraFlags }}
	   - {{ . }}
	   {{- end }}
	*/

	if len(a.targets) != 1 {
		return errors.New("expected one target")
	}
	arg := []string{
		"--httpListenAddr=" + a.agentAddr,
		"--loggerFormat=json",
		"--remoteWrite.showURL",
		"--promscrape.config=http://" + a.addr + "/config",
		"--remoteWrite.url=" + a.targets[0],
	}
	cmd := exec.CommandContext(ctx, "vmagent", arg...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func (a *App) RunPrometheus(ctx context.Context, dir string) error {
	defer func() {
		_ = os.RemoveAll(dir)
	}()
	prometheusConfigFile := filepath.Join(dir, "prometheus.yml")
	if err := os.WriteFile(prometheusConfigFile, a.cfg.Load().marshalYAML(), 0o600); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "prometheus",
		"--config.file="+filepath.Join(dir, "prometheus.yml"),
		"--web.listen-address="+a.agentAddr,
		"--enable-feature=agent",
		"--enable-feature=new-service-discovery-manager",
		"--log.format=json",
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Dir = dir
	go func() {
		// Periodically update the config.
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := os.WriteFile(prometheusConfigFile, a.cfg.Load().marshalYAML(), 0o600); err != nil {
					zctx.From(ctx).Error("cannot update prometheus config", zap.Error(err))
				}
				if err := cmd.Process.Signal(syscall.SIGHUP); err != nil {
					zctx.From(ctx).Error("cannot send SIGHUP to prometheus", zap.Error(err))
				}
			}
		}
	}()
	return cmd.Run()
}

func (a *App) prometheusConfig() *config {
	cfg := newConfig(a.targetsCount, a.scrapeInterval, a.addr)
	if !a.useVictoria {
		var remotes []*remoteWriteConfig
		for i, target := range a.targets {
			remotes = append(remotes, &remoteWriteConfig{
				URL:  target,
				Name: fmt.Sprintf("target-%d", i),
				Metadata: &remoteWriteMetadataConfig{
					Send:         true,
					SendInterval: time.Second,
				},
			})
		}
		cfg.RemoteWrites = remotes
	}
	return cfg
}

func (a *App) parseTargets() {
	for _, arg := range flag.Args() {
		u, err := url.Parse(arg)
		if err != nil {
			fmt.Fprintln(os.Stderr, "invalid target:", err)
			os.Exit(1)
		}
		a.targets = append(a.targets, u.String())
	}
	if len(a.targets) == 0 {
		fmt.Fprintln(os.Stderr, "no targets specified")
		os.Exit(1)
	}
}

func (a *App) run(ctx context.Context, lg *zap.Logger, m *app.Metrics) error {
	a.cfg.Store(a.prometheusConfig())

	g, ctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		return a.ProgressConfig(ctx)
	})
	if a.useVictoria {
		g.Go(func() error {
			return a.RunAgent(ctx)
		})
	} else {
		prometheusDir, err := os.MkdirTemp("", "prometheus")
		if err != nil {
			return err
		}
		g.Go(func() error {
			return a.RunPrometheus(ctx, prometheusDir)
		})
	}
	g.Go(func() error {
		return a.RunNodeExporter(ctx)
	})
	g.Go(func() error {
		a.PollNodeExporter(ctx)
		return nil
	})
	g.Go(func() error {
		mux := http.NewServeMux()
		mux.HandleFunc("/node", a.HandleNodeExporter)
		mux.HandleFunc("/config", a.HandleConfig)
		srv := &http.Server{
			Addr:    a.addr,
			Handler: mux,
		}
		go func() {
			<-ctx.Done()
			_ = srv.Close()
		}()
		if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	})
	return g.Wait()
}

func main() {
	var a App
	flag.StringVar(&a.nodeExporterAddr, "nodeExporterAddr", "127.0.0.1:9301", "address for node exporter to listen")
	flag.StringVar(&a.addr, "addr", "127.0.0.1:8428", "address to listen")
	flag.StringVar(&a.agentAddr, "agentAddr", "127.0.0.1:8429", "address for vmagent to listen")
	flag.IntVar(&a.targetsCount, "targetsCount", 100, "The number of scrape targets to return from -httpListenAddr. Each target has the same address defined by -targetAddr")
	flag.DurationVar(&a.scrapeInterval, "scrapeInterval", time.Second*5, "The scrape_interval to set at the scrape config returned from -httpListenAddr")
	flag.DurationVar(&a.scrapeConfigUpdateInterval, "scrapeConfigUpdateInterval", time.Minute*10, "The -scrapeConfigUpdatePercent scrape targets are updated in the scrape config returned from -httpListenAddr every -scrapeConfigUpdateInterval")
	flag.Float64Var(&a.scrapeConfigUpdatePercent, "scrapeConfigUpdatePercent", 1, "The -scrapeConfigUpdatePercent scrape targets are updated in the scrape config returned from -httpListenAddr ever -scrapeConfigUpdateInterval")
	flag.BoolVar(&a.useVictoria, "useVictoria", true, "use vmagent instead prometheus")
	flag.Parse()
	a.parseTargets()
	app.Run(a.run)
}
