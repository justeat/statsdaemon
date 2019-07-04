package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"runtime/pprof"
	"strings"
	"time"

	"github.com/Dieterbe/profiletrigger/cpu"
	"github.com/Dieterbe/profiletrigger/heap"
	"github.com/raintank/dur"
	"github.com/raintank/statsdaemon"
	"github.com/raintank/statsdaemon/logger"
	"github.com/raintank/statsdaemon/out"
	log "github.com/sirupsen/logrus"

	"net/http"
	_ "net/http/pprof"

	"github.com/grafana/globalconf"
)

const (
	VERSION = "0.6"
	// number of packets we can read out of udp buffer without processing them
	// statsdaemon doesn't really interrupt the udp reader like some other statsd's do (like on flush)
	// but this can still be useful to deal with traffic bursts.
	// keep in mind that one metric is about 30 to 100 bytes of memory.
	MAX_UNPROCESSED_PACKETS = 1000
)

var (
	listen_addr   = flag.String("listen_addr", ":8125", "listener address for statsd, listens on UDP only")
	admin_addr    = flag.String("admin_addr", ":8126", "listener address for admin port")
	profile_addr  = flag.String("profile_addr", "", "listener address for profiler")
	graphite_addr = flag.String("graphite_addr", "127.0.0.1:2003", "graphite carbon-in url")
    prometheus_addr = flag.String("prometheus_addr", ":9091", "where to expose metrics in prometheus format")
	flushInterval = flag.Int("flush_interval", 10, "flush interval in seconds")
	processes     = flag.Int("processes", 2, "number of processes to use")

	instance = flag.String("instance", "$HOST", "instance name, defaults to short hostname if not set")

	legacy_namespace = flag.Bool("legacy_namespace", true, "legacy namespacing (not recommended)")
	prefix_rates     = flag.String("prefix_rates", "stats.", "rates prefix, it is recommended that you use stats.rates if possible")
	prefix_counters  = flag.String("prefix_counters", "stats_counts.", "counters prefix")
	prefix_timers    = flag.String("prefix_timers", "stats.timers.", "timers prefix")
	prefix_gauges    = flag.String("prefix_gauges", "stats.gauges.", "gauges prefix")

	prefix_m20_counters = flag.String("prefix_m20_counters", "", "counters 2.0 prefix")
	prefix_m20_gauges   = flag.String("prefix_m20_gauges", "", "gauges 2.0 prefix")
	prefix_m20_rates    = flag.String("prefix_m20_rates", "", "rates 2.0 prefix")
	prefix_m20_timers   = flag.String("prefix_m20_timers", "", "timers 2.0 prefix")

	flush_rates  = flag.Bool("flush_rates", true, "send count for counters (using prefix_counters)")
	flush_counts = flag.Bool("flush_counts", false, "send count for counters (using prefix_counters)")

	percentile_thresholds = flag.String("percentile_thresholds", "90,75", "percential thresholds (used by timers)")
	max_timers_per_s      = flag.Uint64("max_timers_per_s", 1000, "max timers per second")

	proftrigPath = flag.String("proftrigger_path", "/tmp/profiletrigger/", "profiler file path") // "path to store triggered profiles"

	proftrigHeapFreqStr    = flag.String("proftrigger_heap_freq", "0", "profiler heap frequency")           // "inspect status frequency. set to 0 to disable"
	proftrigHeapMinDiffStr = flag.String("proftrigger_heap_min_diff", "1h", "profiler heap min difference") // "minimum time between triggered profiles"
	proftrigHeapThresh     = flag.Int("proftrigger_heap_thresh", 10000000, "profiler heap threshold")       // "if this many bytes allocated, trigger a profile"

	proftrigCpuFreqStr    = flag.String("proftrigger_cpu_freq", "0", "profiler cpu frequency")           // "inspect status frequency. set to 0 to disable"
	proftrigCpuMinDiffStr = flag.String("proftrigger_cpu_min_diff", "1h", "profiler cpu min difference") // "minimum time between triggered profiles"
	proftrigCpuDurStr     = flag.String("proftrigger_cpu_dur", "5s", "profiler cpu duration")            // "duration of cpu profile"
	proftrigCpuThresh     = flag.Int("proftrigger_cpu_thresh", 80, "profiler cpu threshold")             // "if this much percent cpu used, trigger a profile"

	logLevel    = flag.String("log_level", "info", "log level. panic|fatal|error|warning|info|debug")
	showVersion = flag.Bool("version", false, "print version string")
	config_file = flag.String("config_file", "/etc/statsdaemon.ini", "config file location")
	cpuprofile  = flag.String("cpuprofile", "", "write cpu profile to file")
	memprofile  = flag.String("memprofile", "", "write memory profile to this file")
	GitHash     = "(none)"
)

func expand_cfg_vars(in string) (out string) {
	switch in {
	case "HOST":
		hostname, _ := os.Hostname()
		// in case hostname is an fqdn or has dots, only take first part
		parts := strings.SplitN(hostname, ".", 2)
		return parts[0]
	default:
		return ""
	}
}
func main() {
	flag.Parse()

	if *showVersion {
		fmt.Printf("statsdaemon v%s (built w/%s, git hash %s)\n", VERSION, runtime.Version(), GitHash)
		return
	}
	if *cpuprofile != "" {
		f, err := os.Create(*cpuprofile)
		if err != nil {
			log.Fatal(err)
		}
		pprof.StartCPUProfile(f)
		defer pprof.StopCPUProfile()
	}
	if *memprofile != "" {
		f, err := os.Create(*memprofile)
		if err != nil {
			log.Fatal(err)
		}
		defer f.Close()
		defer pprof.WriteHeapProfile(f)
	}

	path := ""
	if _, err := os.Stat(*config_file); err == nil {
		path = *config_file
	}
	conf, err := globalconf.NewWithOptions(&globalconf.Options{
		Filename:  path,
		EnvPrefix: "SD_",
	})

	conf.ParseAll()

	/***********************************
	          Set up Logger
    ***********************************/

	logformatter := &logger.TextFormatter{}
	logformatter.TimestampFormat = "2006-01-02 15:04:05.000"
	log.SetFormatter(logformatter)
	lvl, err := log.ParseLevel(*logLevel)
	if err != nil {
		log.Fatalf("failed to parse log-level, %s", err.Error())
	}
	log.SetLevel(lvl)
	log.Infof("logging level set to '%s'", *logLevel)

	// TODO: update dur, these functions are deprecated
	proftrigHeapFreq := dur.MustParseUsec("proftrigger_heap_freq", *proftrigHeapFreqStr)
	proftrigHeapMinDiff := int(dur.MustParseUNsec("proftrigger_heap_min_diff", *proftrigHeapMinDiffStr))

	proftrigCpuFreq := dur.MustParseUsec("proftrigger_cpu_freq", *proftrigCpuFreqStr)
	proftrigCpuMinDiff := int(dur.MustParseUNsec("proftrigger_cpu_min_diff", *proftrigCpuMinDiffStr))
	proftrigCpuDur := int(dur.MustParseUNsec("proftrigger_cpu_dur", *proftrigCpuDurStr))

	if proftrigHeapFreq > 0 {
		errors := make(chan error)
		// TODO: update to latest profile trigger
		trigger, _ := heap.New(*proftrigPath, *proftrigHeapThresh, proftrigHeapMinDiff, time.Duration(proftrigHeapFreq)*time.Second, errors)
		go func() {
			for e := range errors {
				log.Errorf("profiletrigger heap: %s", e)
			}
		}()
		go trigger.Run()
	}

	if proftrigCpuFreq > 0 {
		errors := make(chan error)
		freq := time.Duration(proftrigCpuFreq) * time.Second
		duration := time.Duration(proftrigCpuDur) * time.Second
		trigger, _ := cpu.New(*proftrigPath, *proftrigCpuThresh, proftrigCpuMinDiff, freq, duration, errors)
		go func() {
			for e := range errors {
				log.Errorf("profiletrigger cpu: %s", e)
			}
		}()
		go trigger.Run()
	}

	runtime.GOMAXPROCS(*processes)
	pct, err := out.NewPercentiles(*percentile_thresholds)
	if err != nil {
		log.Fatal(err)
	}
	inst := os.Expand(*instance, expand_cfg_vars)
	if inst == "" {
		inst = "null"
	}

	signalchan := make(chan os.Signal, 1)
	signal.Notify(signalchan)
	if *profile_addr != "" {
		go func() {
			log.Info("Profiling endpoint listening on " + *profile_addr)
			log.Info(http.ListenAndServe(*profile_addr, nil))
		}()
	}

	formatter := out.Formatter{
		PrefixInternal: "service_is_statsdaemon.instance_is_" + inst + ".",

		Legacy_namespace: *legacy_namespace,
		Prefix_counters:  *prefix_counters,
		Prefix_gauges:    *prefix_gauges,
		Prefix_rates:     *prefix_rates,
		Prefix_timers:    *prefix_timers,

		Prefix_m20_counters: *prefix_m20_counters,
		Prefix_m20_gauges:   *prefix_m20_gauges,
		Prefix_m20_rates:    *prefix_m20_rates,
		Prefix_m20_timers:   *prefix_m20_timers,

		Prefix_m20ne_counters: strings.Replace(*prefix_m20_counters, "=", "_is_", -1),
		Prefix_m20ne_gauges:   strings.Replace(*prefix_m20_gauges, "=", "_is_", -1),
		Prefix_m20ne_rates:    strings.Replace(*prefix_m20_rates, "=", "_is_", -1),
		Prefix_m20ne_timers:   strings.Replace(*prefix_m20_timers, "=", "_is_", -1),
	}

	daemon := statsdaemon.New(inst, formatter, *flush_rates, *flush_counts, *pct, *flushInterval, MAX_UNPROCESSED_PACKETS, *max_timers_per_s, signalchan)
	if *logLevel == "debug" {
		consumer := make(chan interface{}, 100)
		daemon.Invalid_lines.Register(consumer)
		go func() {
			for line := range consumer {
				log.Debugf("invalid line '%s'", line)
			}
		}()
	}
	daemon.Run(*listen_addr, *admin_addr, *graphite_addr, *prometheus_addr)
}
