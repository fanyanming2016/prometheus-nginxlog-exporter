/*
 * Copyright 2019 Martin Helmich <martin@helmich.me>
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package main

import (
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/martin-helmich/prometheus-nginxlog-exporter/config"
	"github.com/martin-helmich/prometheus-nginxlog-exporter/discovery"
	"github.com/martin-helmich/prometheus-nginxlog-exporter/prof"
	"github.com/martin-helmich/prometheus-nginxlog-exporter/relabeling"
	"github.com/martin-helmich/prometheus-nginxlog-exporter/tail"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/satyrius/gonx"
)

// Metrics is a struct containing pointers to all metrics that should be
// exposed to Prometheus
type Metrics struct {
	countTotal          *prometheus.CounterVec
	bytesTotal          *prometheus.CounterVec
	upstreamSeconds     *prometheus.SummaryVec
	upstreamSecondsHist *prometheus.HistogramVec
	responseSeconds     *prometheus.SummaryVec
	responseSecondsHist *prometheus.HistogramVec
	parseErrorsTotal    prometheus.Counter
}

// Init initializes a metrics struct
func (m *Metrics) Init(cfg *config.NamespaceConfig) {
	cfg.MustCompile()

	labels := cfg.OrderedLabelNames

	for _, r := range relabeling.DefaultRelabelings {
		labels = append(labels, r.TargetLabel)
	}

	for i := range cfg.RelabelConfigs {
		labels = append(labels, cfg.RelabelConfigs[i].TargetLabel)
	}

	m.countTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: cfg.Name,
		Name:      "http_response_count_total",
		Help:      "Amount of processed HTTP requests",
	}, labels)

	m.bytesTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: cfg.Name,
		Name:      "http_response_size_bytes",
		Help:      "Total amount of transferred bytes",
	}, labels)

	m.upstreamSeconds = prometheus.NewSummaryVec(prometheus.SummaryOpts{
		Namespace: cfg.Name,
		Name:      "http_upstream_time_seconds",
		Help:      "Time needed by upstream servers to handle requests",
	}, labels)

	m.upstreamSecondsHist = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: cfg.Name,
		Name:      "http_upstream_time_seconds_hist",
		Help:      "Time needed by upstream servers to handle requests",
	}, labels)

	m.responseSeconds = prometheus.NewSummaryVec(prometheus.SummaryOpts{
		Namespace: cfg.Name,
		Name:      "http_response_time_seconds",
		Help:      "Time needed by NGINX to handle requests",
	}, labels)

	m.responseSecondsHist = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: cfg.Name,
		Name:      "http_response_time_seconds_hist",
		Help:      "Time needed by NGINX to handle requests",
	}, labels)

	m.parseErrorsTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: cfg.Name,
		Name:      "parse_errors_total",
		Help:      "Total number of log file lines that could not be parsed",
	})

	prometheus.MustRegister(m.countTotal)
	prometheus.MustRegister(m.bytesTotal)
	prometheus.MustRegister(m.upstreamSeconds)
	prometheus.MustRegister(m.upstreamSecondsHist)
	prometheus.MustRegister(m.responseSeconds)
	prometheus.MustRegister(m.responseSecondsHist)
	prometheus.MustRegister(m.parseErrorsTotal)
}

func main() {
	var opts config.StartupFlags
	var cfg = config.Config{
		Listen: config.ListenConfig{
			Port:    4040,
			Address: "0.0.0.0",
		},
	}

	flag.IntVar(&opts.ListenPort, "listen-port", 4040, "HTTP port to listen on")
	flag.StringVar(&opts.Format, "format", `$remote_addr - $remote_user [$time_local] "$request" $status $body_bytes_sent "$http_referer" "$http_user_agent" "$http_x_forwarded_for"`, "NGINX access log format")
	flag.StringVar(&opts.Namespace, "namespace", "nginx", "namespace to use for metric names")
	flag.StringVar(&opts.ConfigFile, "config-file", "", "Configuration file to read from")
	flag.BoolVar(&opts.EnableExperimentalFeatures, "enable-experimental", false, "Set this flag to enable experimental features")
	flag.StringVar(&opts.CPUProfile, "cpuprofile", "", "write cpu profile to `file`")
	flag.StringVar(&opts.MemProfile, "memprofile", "", "write memory profile to `file`")
	flag.Parse()

	opts.Filenames = flag.Args()

	sigChan := make(chan os.Signal, 1)
	stopChan := make(chan bool)
	stopHandlers := sync.WaitGroup{}

	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGINT)

	go func() {
		sig := <-sigChan

		fmt.Printf("caught term %s. exiting\n", sig)

		close(stopChan)
		stopHandlers.Wait()

		os.Exit(0)
	}()

	prof.SetupCPUProfiling(opts.CPUProfile, stopChan, &stopHandlers)
	prof.SetupMemoryProfiling(opts.MemProfile, stopChan, &stopHandlers)

	loadConfig(&opts, &cfg)

	fmt.Printf("using configuration %+v\n", cfg)

	if stabilityError := cfg.StabilityWarnings(); stabilityError != nil && !opts.EnableExperimentalFeatures {
		fmt.Fprintf(os.Stderr, "Your configuration file contains an option that is explicitly labeled as experimental feature:\n\n  %s\n\n", stabilityError.Error())
		fmt.Fprintln(os.Stderr, "Use the -enable-experimental flag or the enable_experimental option to enable these features. Use them at your own peril.")

		os.Exit(1)
	}

	if cfg.Consul.Enable {
		setupConsul(&cfg, stopChan, &stopHandlers)
	}

	for _, ns := range cfg.Namespaces {
		fmt.Printf("starting listener for namespace %s\n", ns.Name)

		go processNamespace(ns)
	}

	listenAddr := fmt.Sprintf("%s:%d", cfg.Listen.Address, cfg.Listen.Port)
	fmt.Printf("running HTTP server on address %s\n", listenAddr)

	http.Handle("/metrics", prometheus.Handler())
	http.ListenAndServe(listenAddr, nil)
}

func loadConfig(opts *config.StartupFlags, cfg *config.Config) {
	if opts.ConfigFile != "" {
		fmt.Printf("loading configuration file %s\n", opts.ConfigFile)
		if err := config.LoadConfigFromFile(cfg, opts.ConfigFile); err != nil {
			panic(err)
		}
	} else if err := config.LoadConfigFromFlags(cfg, opts); err != nil {
		panic(err)
	}
}

func setupConsul(cfg *config.Config, stopChan <-chan bool, stopHandlers *sync.WaitGroup) {
	registrator, err := discovery.NewConsulRegistrator(cfg)
	if err != nil {
		panic(err)
	}

	fmt.Printf("registering service in Consul\n")
	if err := registrator.RegisterConsul(); err != nil {
		panic(err)
	}

	go func() {
		<-stopChan
		fmt.Printf("unregistering service in Consul\n")
		registrator.UnregisterConsul()

		stopHandlers.Done()
	}()

	stopHandlers.Add(1)
}

func processNamespace(nsCfg config.NamespaceConfig) {
	parser := gonx.NewParser(nsCfg.Format)

	metrics := Metrics{}
	metrics.Init(&nsCfg)

	for _, f := range nsCfg.SourceFiles {
		t, err := tail.NewFollower(f)
		if err != nil {
			panic(err)
		}

		t.OnError(func(err error) {
			panic(err)
		})

		go processSourceFile(nsCfg, t, parser, &metrics)
	}
}

func processSourceFile(nsCfg config.NamespaceConfig, t tail.Follower, parser *gonx.Parser, metrics *Metrics) {
	relabelings := relabeling.NewRelabelings(nsCfg.RelabelConfigs)
	relabelings = append(relabeling.DefaultRelabelings, relabelings...)

	staticLabelValues := nsCfg.OrderedLabelValues

	totalLabelCount := len(staticLabelValues) + len(relabelings)
	relabelLabelOffset := len(staticLabelValues)
	labelValues := make([]string, totalLabelCount)

	for i := range staticLabelValues {
		labelValues[i] = staticLabelValues[i]
	}

	for line := range t.Lines() {
		entry, err := parser.ParseString(line.Text)
		if err != nil {
			fmt.Printf("error while parsing line '%s': %s\n", line.Text, err)
			metrics.parseErrorsTotal.Inc()
			continue
		}

		for i := range relabelings {
			if str, err := entry.Field(relabelings[i].SourceValue); err == nil {
				mapped, err := relabelings[i].Map(str)
				if err == nil {
					labelValues[i+relabelLabelOffset] = mapped
				}
			}
		}

		metrics.countTotal.WithLabelValues(labelValues...).Inc()

		if bytes, err := entry.FloatField("body_bytes_sent"); err == nil {
			metrics.bytesTotal.WithLabelValues(labelValues...).Add(bytes)
		}

		if upstreamTime, err := entry.FloatField("upstream_response_time"); err == nil {
			metrics.upstreamSeconds.WithLabelValues(labelValues...).Observe(upstreamTime)
			metrics.upstreamSecondsHist.WithLabelValues(labelValues...).Observe(upstreamTime)
		}

		if responseTime, err := entry.FloatField("request_time"); err == nil {
			metrics.responseSeconds.WithLabelValues(labelValues...).Observe(responseTime)
			metrics.responseSecondsHist.WithLabelValues(labelValues...).Observe(responseTime)
		}
	}
}
