package main

import (
	"flag"
	"fmt"
	"github.com/dustin/go-humanize"
	"github.com/fatih/color"
	"github.com/safchain/ethtool"
	"html/template"
	"k8s.io/klog/v2"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

var (
	flagInterface   = flag.String("i", "", "interface name")
	flagRefreshRate = flag.Duration("s", 1*time.Second, "refresh rate")
	flagShowQueues  = flag.Bool("q", false, "show queue stats")
)

func init() {
	klog.InitFlags(nil)
	flag.Parse()
	if *flagInterface == "" {
		klog.Exit("missing required flag: -i")
	}
}

func main() {
	klog.Infof("Running on interface %s", *flagInterface)

	// Catch SIGINT and SIGTERM to exit gracefully
	signalCh := make(chan os.Signal, 1)
	signal.Notify(signalCh, syscall.SIGINT, syscall.SIGTERM)

	// Timer
	ticker := time.NewTicker(*flagRefreshRate)
	defer ticker.Stop()

	// Netlink socket
	et, err := ethtool.NewEthtool()
	if err != nil {
		klog.Exitf("create netlink socket: %v", err)
	}

	var lastStats map[string]uint64

	for {
		select {
		case <-signalCh:
			klog.Info("Received signal, exiting...")
			// Reset terminal to normal mode and enable cursor
			if !klog.V(1).Enabled() {
				fmt.Print("\033[?25h\033[0m")
			}
			return
		case <-ticker.C:
			stats, err := et.Stats(*flagInterface)
			if err != nil {
				klog.Errorf("get stats: %v", err)
				return
			}
			if klog.V(2).Enabled() {
				printStats(stats)
			}
			if lastStats == nil {
				lastStats = stats
				continue
			}

			// Calculate the delta
			delta := make(map[string]uint64)
			for key, value := range stats {
				delta[key] = value - lastStats[key]
			}

			if klog.V(1).Enabled() {
				printStats(delta)
			}

			// Collect all "port.rx_size_<n>" keys
			type sizeKeys struct {
				Key  string
				Size uint64
			}
			rxSizeKeys := make([]sizeKeys, 0)

			for key := range delta {
				if strings.HasPrefix(key, "port.rx_size_") {
					k := key[13:]
					if err != nil {
						panic(err)
					}
					rxSizeKeys = append(rxSizeKeys, sizeKeys{k, delta[key]})
				}
			}
			sort.SliceStable(rxSizeKeys, func(i, j int) bool {
				if rxSizeKeys[i].Key == "big" {
					return false
				}
				if rxSizeKeys[j].Key == "big" {
					return true
				}

				ii, err := strconv.Atoi(rxSizeKeys[i].Key)
				if err != nil {
					panic(err)
				}
				jj, err := strconv.Atoi(rxSizeKeys[j].Key)
				if err != nil {
					panic(err)
				}
				return ii < jj
			})

			// Collect rx/tx queue stats
			//  rx-38.bytes, rx-38.packets, tx-38.bytes, tx-38.packets...
			type queue struct {
				RxBytes   uint64
				RxPackets uint64
				TxBytes   uint64
				TxPackets uint64
			}

			numQueues := 0
			for key := range delta {
				if strings.HasPrefix(key, "rx-") && strings.HasSuffix(key, ".bytes") {
					numQueues++
				}
			}

			klog.V(1).Infof("numQueues: %d", numQueues)

			qs := make([]queue, numQueues)
			for key, value := range delta {
				if strings.HasPrefix(key, "rx-") {
					q, err := strconv.Atoi(key[3:strings.Index(key, ".")])
					if err != nil {
						panic(err)
					}
					if strings.HasSuffix(key, ".bytes") {
						qs[q].RxBytes = value
					}
					if strings.HasSuffix(key, ".packets") {
						qs[q].RxPackets = value
					}
				}
				if strings.HasPrefix(key, "tx-") {
					q, err := strconv.Atoi(key[3:strings.Index(key, ".")])
					if err != nil {
						panic(err)
					}
					if strings.HasSuffix(key, ".bytes") {
						qs[q].TxBytes = value
					}
					if strings.HasSuffix(key, ".packets") {
						qs[q].TxPackets = value
					}
				}
			}

			// Sort by queue number
			sort.SliceStable(qs, func(i, j int) bool {
				return i < j
			})

			tt := template.New("stats")
			tt.Funcs(template.FuncMap{
				"pps": func(n uint64) string {
					f, unit := humanize.ComputeSI(float64(n))
					s := fmt.Sprintf("%.02f %spps", f, unit)
					if f > 0 {
						return color.BlueString(s)
					}
					return color.HiWhiteString(s)
				},
				"bps": func(n uint64) string {
					f, unit := humanize.ComputeSI(float64(n * 8))
					s := fmt.Sprintf("%.02f %sbps", f, unit)
					if f > 0 {
						return color.GreenString(s)
					}
					return color.HiWhiteString(s)
				},
				"add": func(a, b uint64) uint64 {
					return a + b
				},
				"red": func(s string) string {
					return color.RedString(s)
				},
				"pad": func(s string, n int) string {
					return fmt.Sprintf("%-*s", n, s)
				},
				"top": func() string {
					if klog.V(1).Enabled() {
						return ""
					}
					// Set to alternate screen and return cursor to home
					return "\x1b[?1049h\x1b[H"
				},
				"tx_port": func(s string) string {
					return fmt.Sprintf("port.tx_size_%s", s)
				},
			})
			tpl, err := tt.Parse(out)
			if err != nil {
				panic(err)
			}

			if !klog.V(1).Enabled() {
				// Clear the screen
				fmt.Printf("\x1b[2J\x1b[H")
				// Disable cursor
				fmt.Printf("\x1b[?25l")
			}

			err = tpl.Execute(os.Stdout, struct {
				S          map[string]uint64
				RxHist     []sizeKeys
				Qs         []queue
				ShowQueues bool
			}{
				S:          delta,
				RxHist:     rxSizeKeys,
				Qs:         qs,
				ShowQueues: *flagShowQueues,
			})
			if err != nil {
				panic(err)
			}

			lastStats = stats
		}
	}
}

const out = `{{ top }}
Total Rx:
  Bytes:   {{ index .S "port.rx_bytes" | bps }}
  Packets: {{ index .S "rx_packets" | pps }}
  Dropped: {{ index .S "port.rx_dropped" | pps }}
	
  All: {{ add (index .S "rx_packets") (index .S "port.rx_dropped") | pps }}

Total Tx:
  Bytes:   {{ index .S "port.tx_bytes" | bps }}
  Packets: {{ index .S "tx_packets" | pps }}
  Dropped: {{ index .S "port.tx_dropped" | pps }}
	
  All: {{ add (index .S "tx_packets") (index .S "port.tx_dropped") | pps }}

Rx/Tx distribution

  Q     | Rx PPS   | Tx PPS
 -----------------------------
{{- range .RxHist }}
  {{ pad .Key 5 }} | {{ pad (.Size | pps) 12 }} | {{ index $.S (tx_port .Key) | pps }}
{{- end }}
{{ if .ShowQueues }}
Queue stats (tx queue stats not always available):

  Q    : Rx Bytes     Rx Packets     |   Tx Bytes     Tx Packets
------------------------------------------------------------------
{{- range $i, $q := .Qs }}
  {{ pad (printf "%d" $i) 5 }}: {{ pad ($q.RxBytes | bps) 12 }} {{ pad ($q.RxPackets | pps) 12 }}   |   {{ pad ($q.TxBytes | bps) 12 }} {{ pad ($q.TxPackets | pps) 12 }}
{{- end }}
{{- end }}
`

// printStats prints a text-aligned and sorted table of stats to stdout.
func printStats(stats map[string]uint64) {
	// Find the longest Key
	var maxKeyLen int
	for key := range stats {
		if len(key) > maxKeyLen {
			maxKeyLen = len(key)
		}
	}

	// Sort
	var keys []string
	for key := range stats {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	// Print the table
	for _, key := range keys {
		if stats[key] == 0 {
			continue
		}
		klog.Infof("%-*s %d", maxKeyLen, key, stats[key])
	}
}
