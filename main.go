package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

var verbose bool

func main() {
	flag.BoolVar(&verbose, "verbose", false, "reports progress. disabled by default (optional)")
	from := flag.Int("from", 0, "from year (required)")
	to := flag.Int("to", 0, "to year. equals to from year if it is 0 or count is 0 (optional)")
	count := flag.Int("count", 0, "count of years. used to calc to year if to year is 0 (optional)")
	output := flag.String("output", "download", "output folder for downloaded resources (optional)")
	flag.Parse()

	ctx, cancel := context.WithCancel(context.Background())

	go func() {
		sigchan := make(chan os.Signal, 1)
		signal.Notify(sigchan, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT, os.Interrupt)
		select {
		case <-sigchan:
			cancel()
			return
		case <-ctx.Done():
			return
		}
	}()

	downloader, err := NewDownloader(*from, *to, *count, *output)
	if err != nil {
		fmt.Printf("error on startup %v\n", err)
		cancel()
		defer os.Exit(1)
		return
	}

	done := downloader.Run(ctx)
	printStart(downloader)

loop:
	for {
		select {
		case <-time.Tick(progressInterval()):
			printStatus(downloader)
			break
		case <-done:
			cancel()
			break loop
		case <-ctx.Done():
			break loop
		}
	}

	fmt.Printf("\n--------------------------\n")
	printStatus(downloader)

	exitCode := 0
	err = downloader.Err()
	if err != nil {
		exitCode = 1
		fmt.Printf("\nprocessing stopped on error %v\n", err)
		printErrStack(err)
	}
	defer os.Exit(exitCode)
}

func printErrStack(err error) {
	if !verbose || err == nil {
		return
	}
	if fe, ok := err.(*FatalError); ok {
		fmt.Println(fe.ErrorStack())
	}
}

func printStart(d Downloader) {
	totYears := d.YearTo() - d.YearFrom() + 1
	var sb strings.Builder
	sb.WriteString("started downloading for ")
	if totYears > 1 {
		sb.WriteString(fmt.Sprintf("%v years from %v to %v", totYears, d.YearFrom(), d.YearTo()))
	} else {
		sb.WriteString(fmt.Sprintf("%v year", d.YearFrom()))
	}
	fmt.Printf("%v %v\n", formatNow(), sb.String())
}

func printStatus(d Downloader) {
	fmt.Printf("%v %v\n", formatNow(), d.Status())
}

func formatNow() string {
	return time.Now().Format("03:04:05.000")
}

func progressInterval() time.Duration {
	if verbose {
		return 15 * time.Second
	}
	return 30 * time.Second
}
