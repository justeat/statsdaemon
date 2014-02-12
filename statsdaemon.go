package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"os"
	"os/signal"
	"runtime"
	"runtime/pprof"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/stvp/go-toml-config"
)

const (
	VERSION                 = "0.5.2-alpha"
	MAX_UNPROCESSED_PACKETS = 1000
	MAX_UDP_PACKET_SIZE     = 512
)

var signalchan chan os.Signal

type Packet struct {
	Bucket   string
	Value    float64
	Modifier string
	Sampling float32
}

// an amount of 1 per instance is imlpied
type SubmitAmount struct {
	Bucket   string
	Sampling float32
}

type Float64Slice []float64

type TimerData struct {
	Points           Float64Slice
	Amount_submitted int64
}

func (s Float64Slice) Len() int           { return len(s) }
func (s Float64Slice) Swap(i, j int)      { s[i], s[j] = s[j], s[i] }
func (s Float64Slice) Less(i, j int) bool { return s[i] < s[j] }

type Percentiles []*Percentile
type Percentile struct {
	float float64
	str   string
}

func (a *Percentiles) Set(s string) error {
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return err
	}
	*a = append(*a, &Percentile{f, strings.Replace(s, ".", "_", -1)})
	return nil
}
func (p *Percentile) String() string {
	return p.str
}
func (a *Percentiles) String() string {
	return fmt.Sprintf("%v", *a)
}

var (
	listen_addr          = config.String("listen_addr", ":8125")
	admin_addr           = config.String("admin_addr", ":8126")
	graphite_addr        = config.String("graphite_addr", "127.0.0.1:2003")
	flushInterval        = config.Int("flush_interval", 10)
	prefix_rates         = config.String("prefix_rates", "stats.")
	prefix_timers        = config.String("prefix_timers", "stats.timers.")
	prefix_gauges        = config.String("prefix_gauges", "stats.gauges.")
	percentile_tresholds = config.String("percentile_tresholds", "")
	percentThreshold     = Percentiles{}
	max_timers_per_s     = config.Uint64("max_timers_per_s", 1000)

	debug       = flag.Bool("debug", false, "print statistics sent to graphite")
	showVersion = flag.Bool("version", false, "print version string")
	config_file = flag.String("config_file", "/etc/statsdaemon.ini", "config file location")
	cpuprofile  = flag.String("cpuprofile", "", "write cpu profile to file")
	memprofile  = flag.String("memprofile", "", "write memory profile to this file")
)

type metricsSeenReq struct {
	Bucket string
	Conn   *net.Conn
}

var (
	Metrics            = make(chan *Packet, MAX_UNPROCESSED_PACKETS)
	metricsSeen        = make(chan SubmitAmount)
	idealSampleRateReq = make(chan metricsSeenReq)
	counters           = make(map[string]float64)
	gauges             = make(map[string]float64)
	timers             = make(map[string]TimerData)
)

func metricsMonitor() {
	period := time.Duration(*flushInterval) * time.Second
	ticker := time.NewTicker(period)
	for {
		select {
		case sig := <-signalchan:
			switch sig {
			case syscall.SIGTERM, syscall.SIGINT:
				fmt.Printf("!! Caught signal %d... shutting down\n", sig)
				if err := submit(time.Now().Add(period)); err != nil {
					log.Printf("ERROR: %s", err)
				}
				return
			default:
				fmt.Printf("unknown signal %d, ignoring\n", sig)
			}
		case <-ticker.C:
			if err := submit(time.Now().Add(period)); err != nil {
				log.Printf("ERROR: %s", err)
			}
		case s := <-Metrics:
			if s.Modifier == "ms" {
				_, ok := timers[s.Bucket]
				if !ok {
					var p Float64Slice
					timers[s.Bucket] = TimerData{p, 0}
				}
				t := timers[s.Bucket]
				t.Points = append(t.Points, s.Value)
				t.Amount_submitted += int64(1 / s.Sampling)
				timers[s.Bucket] = t
			} else if s.Modifier == "g" {
				gauges[s.Bucket] = s.Value
			} else {
				v, ok := counters[s.Bucket]
				if !ok || v < 0 {
					counters[s.Bucket] = 0
				}
				counters[s.Bucket] += s.Value * float64(1/s.Sampling)
			}
		}
	}
}

type processFn func(*bytes.Buffer, int64, Percentiles) int64

func instrument(fun processFn, buffer *bytes.Buffer, now int64, pctls Percentiles, name string) (num int64) {
	time_start := time.Now()
	num = fun(buffer, now, pctls)
	time_end := time.Now()
	duration_ms := float64(time_end.Sub(time_start).Nanoseconds()) / float64(1000000)
	log.Printf("stats.statsdaemon.%s.type=%s.what=calculation.unit=ms %f %d\n", "dfvimeographite3", name, duration_ms, now)
	log.Printf("stats.statsdaemon.%s.%s.type=%s.direction=out.unit=metrics %d %d\n", "dfvimeographite3", *graphite_addr, name, num, now)
	return
}

func submit(deadline time.Time) error {
	var buffer bytes.Buffer
	var num int64

	now := time.Now().Unix()

	client, err := net.Dial("tcp", *graphite_addr)
	if err != nil {
		if *debug {
			log.Printf("WARNING: resetting counters when in debug mode")
			processCounters(&buffer, now, percentThreshold)
			processGauges(&buffer, now, percentThreshold)
			processTimers(&buffer, now, percentThreshold)
		}
		errmsg := fmt.Sprintf("dialing %s failed - %s", *graphite_addr, err)
		return errors.New(errmsg)
	}
	defer client.Close()

	err = client.SetDeadline(deadline)
	if err != nil {
		errmsg := fmt.Sprintf("could not set deadline:", err)
		return errors.New(errmsg)
	}
	num += instrument(processCounters, &buffer, now, percentThreshold, "counters")
	num += instrument(processGauges, &buffer, now, percentThreshold, "gauges")
	num += instrument(processTimers, &buffer, now, percentThreshold, "timers")
	if num == 0 {
		return nil
	}

	if *debug {
		for _, line := range bytes.Split(buffer.Bytes(), []byte("\n")) {
			if len(line) == 0 {
				continue
			}
			log.Printf("DEBUG: %s", line)
		}
	}

	_, err = client.Write(buffer.Bytes())
	if err != nil {
		errmsg := fmt.Sprintf("failed to write stats - %s", err)
		return errors.New(errmsg)
	}

	//fmt.Println("end of submit")
	//fmt.Fprintf(&buffer, ...
	return nil
}

func processCounters(buffer *bytes.Buffer, now int64, pctls Percentiles) int64 {
	var num int64
	for s, c := range counters {
		counters[s] = -1
		v := c / float64(*flushInterval)
		fmt.Fprintf(buffer, "%s%s %f %d\n", *prefix_rates, s, v, now)
		num++
		delete(counters, s)
	}
	//counters = make(map[string]float64) this should be better than deleting every single entry
	return num
}

func processGauges(buffer *bytes.Buffer, now int64, pctls Percentiles) int64 {
	var num int64
	for g, c := range gauges {
		if c == math.MaxUint64 {
			continue
		}
		fmt.Fprintf(buffer, "%s%s %f %d\n", *prefix_gauges, g, c, now)
		gauges[g] = math.MaxUint64
		num++
	}
	return num
}

func processTimers(buffer *bytes.Buffer, now int64, pctls Percentiles) int64 {
	// these are the metrics that get exposed:
	// count estimate of original amount of metrics sent, by dividing received by samplerate
	// count_ps  same but per second
	// lower
	// mean  // arithmetic mean
	// mean_<pct> // arithmetic mean of values below <pct> percentile
	// median
	// std  standard deviation
	// sum
	// sum_90
	// upper
	// upper_90 / lower_90

	var num int64
	for u, t := range timers {
		if len(t.Points) > 0 {
			seen := len(t.Points)
			count := t.Amount_submitted
			count_ps := float64(count) / float64(*flushInterval)
			num++

			sort.Sort(t.Points)
			min := t.Points[0]
			max := t.Points[seen-1]

			sum := float64(0)
			for _, value := range t.Points {
				sum += value
			}
			mean := float64(sum) / float64(seen)
			sumOfDiffs := float64(0)
			for _, value := range t.Points {
				sumOfDiffs += math.Pow((float64(value) - mean), 2)
			}
			stddev := math.Sqrt(sumOfDiffs / float64(seen))
			mid := seen / 2
			var median float64
			if seen%2 == 1 {
				median = t.Points[mid]
			} else {
				median = (t.Points[mid-1] + t.Points[mid]) / 2
			}
			var cumulativeValues Float64Slice
			cumulativeValues = make(Float64Slice, seen, seen)
			cumulativeValues[0] = t.Points[0]
			for i := 1; i < seen; i++ {
				cumulativeValues[i] = t.Points[i] + cumulativeValues[i-1]
			}

			maxAtThreshold := max
			sum_pct := sum
			mean_pct := mean

			for _, pct := range pctls {

				if seen > 1 {
					var abs float64
					if pct.float >= 0 {
						abs = pct.float
					} else {
						abs = 100 + pct.float
					}
					// poor man's math.Round(x):
					// math.Floor(x + 0.5)
					indexOfPerc := int(math.Floor(((abs / 100.0) * float64(seen)) + 0.5))
					if pct.float >= 0 {
						sum_pct = cumulativeValues[indexOfPerc-1]
						maxAtThreshold = t.Points[indexOfPerc-1]
					} else {
						maxAtThreshold = t.Points[indexOfPerc]
						sum_pct = cumulativeValues[seen-1] - cumulativeValues[seen-indexOfPerc-1]
					}
					mean_pct = float64(sum_pct) / float64(indexOfPerc)
				}

				var tmpl string
				var pctstr string
				if pct.float >= 0 {
					tmpl = "%s%s.upper_%s %f %d\n"
					pctstr = pct.str
				} else {
					tmpl = "%s%s.lower_%s %f %d\n"
					pctstr = pct.str[1:]
				}
				fmt.Fprintf(buffer, tmpl, *prefix_timers, u, pctstr, maxAtThreshold, now)
				fmt.Fprintf(buffer, "%s%s.mean_%s %f %d\n", *prefix_timers, u, pctstr, mean_pct, now)
				fmt.Fprintf(buffer, "%s%s.sum_%s %f %d\n", *prefix_timers, u, pctstr, sum_pct, now)
			}

			var z Float64Slice
			timers[u] = TimerData{z, 0}

			fmt.Fprintf(buffer, "%s%s.mean %f %d\n", *prefix_timers, u, mean, now)
			fmt.Fprintf(buffer, "%s%s.median %f %d\n", *prefix_timers, u, median, now)
			fmt.Fprintf(buffer, "%s%s.std %f %d\n", *prefix_timers, u, stddev, now)
			fmt.Fprintf(buffer, "%s%s.sum %f %d\n", *prefix_timers, u, sum, now)
			fmt.Fprintf(buffer, "%s%s.upper %f %d\n", *prefix_timers, u, max, now)
			fmt.Fprintf(buffer, "%s%s.lower %f %d\n", *prefix_timers, u, min, now)
			fmt.Fprintf(buffer, "%s%s.count %d %d\n", *prefix_timers, u, count, now)
			fmt.Fprintf(buffer, "%s%s.count_ps %f %d\n", *prefix_timers, u, count_ps, now)
		}
	}
	return num
}

func parseMessage(data []byte) []*Packet {
	var output []*Packet
	for _, line := range bytes.Split(data, []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		parts := bytes.SplitN(line, []byte(":"), 2)
		if len(parts) != 2 {
			if *debug {
				log.Printf("invalid line '%s'\n", line)
			}
			continue
		}
		if bytes.Contains(parts[1], []byte(":")) {
			if *debug {
				log.Printf("invalid line '%s'\n", line)
			}
			continue
		}
		bucket := parts[0]
		parts = bytes.SplitN(parts[1], []byte("|"), 3)
		if len(parts) < 2 {
			if *debug {
				log.Printf("invalid line '%s'\n", line)
			}
			continue
		}
		modifier := string(parts[1])
		if modifier != "g" && modifier != "c" && modifier != "ms" {
			if *debug {
				log.Printf("invalid line '%s'\n", line)
			}
			continue
		}
		sampleRate := float64(1)
		if len(parts) == 3 {
			if parts[2][0] != byte('@') {
				if *debug {
					log.Printf("invalid line '%s'\n", line)
				}
				continue
			}
			var err error
			sampleRate, err = strconv.ParseFloat(string(parts[2])[1:], 32)
			if err != nil {
				if *debug {
					log.Printf("invalid line '%s'\n", line)
				}
				continue
			}
		}
		value, err := strconv.ParseFloat(string(parts[0]), 64)
		if err != nil {
			log.Printf("ERROR: failed to parse value in line '%s' - %s\n", line, err)
			continue
		}
		packet := &Packet{
			Bucket:   string(bucket),
			Value:    value,
			Modifier: modifier,
			Sampling: float32(sampleRate),
		}
		output = append(output, packet)
	}
	return output
}

func udpListener() {
	address, _ := net.ResolveUDPAddr("udp", *listen_addr)
	log.Printf("listening on %s", address)
	listener, err := net.ListenUDP("udp", address)
	if err != nil {
		log.Fatalf("ERROR: ListenUDP - %s", err)
	}
	defer listener.Close()

	message := make([]byte, MAX_UDP_PACKET_SIZE)
	for {
		n, remaddr, err := listener.ReadFromUDP(message)
		if err != nil {
			log.Printf("ERROR: reading UDP packet from %+v - %s", remaddr, err)
			continue
		}

		for _, p := range parseMessage(message[:n]) {
			Metrics <- p
			metricsSeen <- SubmitAmount{p.Bucket, p.Sampling}
		}
	}
}

// submitted is "triggered" inside statsd client libs, not necessarily sent
// after sampling, network loss and udp packet drops, the amount we see is Seen
type Amounts struct {
	Submitted uint64
	Seen      uint64
}

func metricsSeenMonitor() {
	period := 10 * time.Second
	ticker := time.NewTicker(period)
	// use two maps so we always have enough data shortly after we start a new period
	// counts would be too low and/or too inaccurate otherwise
	_countsA := make(map[string]*Amounts)
	_countsB := make(map[string]*Amounts)
	cur_counts := &_countsA
	prev_counts := &_countsB
	var swap_ts time.Time
	for {
		select {
		case <-ticker.C:
			prev_counts = cur_counts
			new_counts := make(map[string]*Amounts)
			cur_counts = &new_counts
			swap_ts = time.Now()
		case s_a := <-metricsSeen:
			el, ok := (*cur_counts)[s_a.Bucket]
			if ok {
				el.Seen += 1
				el.Submitted += uint64(1 / s_a.Sampling)
			} else {
				(*cur_counts)[s_a.Bucket] = &Amounts{1, uint64(1 / s_a.Sampling)}
			}
		case req := <-idealSampleRateReq:
			current_ts := time.Now()
			interval := current_ts.Sub(swap_ts).Seconds() + 10
			submitted := uint64(0)
			el, ok := (*cur_counts)[req.Bucket]
			if ok {
				submitted += el.Submitted
			}
			el, ok = (*prev_counts)[req.Bucket]
			if ok {
				submitted += el.Submitted
			}
			submitted_per_s := submitted / uint64(interval)
			// submitted (at source) per second * ideal_sample_rate should be ~= *max_timers_per_s
			ideal_sample_rate := float32(1)
			if submitted_per_s > *max_timers_per_s {
				ideal_sample_rate = float32(*max_timers_per_s) / float32(submitted_per_s)
			}
			resp := fmt.Sprintf("%s %f\n", req.Bucket, ideal_sample_rate)
			go handleApiRequest(*req.Conn, []byte(resp))
		}
	}
}

func writeHelp(conn net.Conn) {
	help := `
    commands:
        ideal_sample_rate <metric key>   get the ideal sample rate for given metric
        help                             show this menu

`
	conn.Write([]byte(help))
}

func handleApiRequest(conn net.Conn, write_first []byte) {
	conn.Write(write_first)
	// Make a buffer to hold incoming data.
	buf := make([]byte, 1024)
	// Read the incoming connection into the buffer.
	for {
		n, err := conn.Read(buf)
		if err != nil {
			if err == io.EOF {
				fmt.Println("read eof. closing")
				conn.Close()
				break
			} else {
				fmt.Println("Error reading:", err.Error())
			}
		}
		clean_cmd := strings.TrimSpace(string(buf[:n]))
		command := strings.Split(clean_cmd, " ")
		if *debug {
			fmt.Println("received command: '" + clean_cmd + "'")
		}
		switch command[0] {
		case "ideal_sample_rate":
			if len(command) != 2 {
				conn.Write([]byte("invalid request\n"))
				writeHelp(conn)
				continue
			}
			idealSampleRateReq <- metricsSeenReq{command[1], &conn}
			return
		case "help":
			writeHelp(conn)
			continue
		default:
			conn.Write([]byte("unknown command\n"))
			writeHelp(conn)
		}
	}
}
func adminListener() {
	l, err := net.Listen("tcp", *admin_addr)
	if err != nil {
		fmt.Println("Error listening:", err.Error())
		os.Exit(1)
	}
	defer l.Close()
	fmt.Println("Listening on " + *admin_addr)
	for {
		// Listen for an incoming connection.
		conn, err := l.Accept()
		if err != nil {
			fmt.Println("Error accepting: ", err.Error())
			os.Exit(1)
		}
		go handleApiRequest(conn, []byte(""))
	}
}

func main() {
	flag.Parse()

	if *showVersion {
		fmt.Printf("statsdaemon v%s (built w/%s)\n", VERSION, runtime.Version())
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
	config.Parse(*config_file)
	pcts := strings.Split(*percentile_tresholds, ",")
	for _, pct := range pcts {
		percentThreshold.Set(pct)
	}

	signalchan = make(chan os.Signal, 1)
	signal.Notify(signalchan)

	go udpListener()
	go adminListener()
	go metricsSeenMonitor()
	metricsMonitor()
}
