package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	from := flag.Int("from", 0, "from year (required)")
	to := flag.Int("to", 0, "to year. equals to from year if it is 0 or count is 0 (optional)")
	count := flag.Int("count", 0, "count of years. used to calc to year if to year is 0 (optional)")
	verbose := flag.Bool("verbose", false, "reports progress. disabled by default (optional)")
	output := flag.String("output", "download", "output folder for downloaded resources (optional)")
	flag.Parse()

	ctx, cancel := context.WithCancel(context.Background())

	go func() {
		sigchan := make(chan os.Signal, 1)
		signal.Notify(sigchan, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT, os.Interrupt)
		select {
		case sig := <-sigchan:
			fmt.Printf("received system signal %+v \ncancelling main context and quit\n", sig)
			cancel()
			return
		case <-ctx.Done():
			return
		}
	}()

	downloader, err := NewDownloader(*from, *to, *count, *verbose, *output)
	if err != nil {
		fmt.Printf("error on startup %v\n", err)
		cancel()
		defer os.Exit(1)
		return
	}

	done := downloader.Run(ctx)
	select {
	case <-done:
		cancel()
		break
	case <-ctx.Done():
		break
	}

	fmt.Printf("\n--------------------------\n")
	exitCode := 0
	err = downloader.Err()
	if err != nil {
		exitCode = 1
		fmt.Printf("\nprocessing stopped on error %v\n", err)
		if *verbose {
			if fe, ok := err.(*FatalError); ok {
				fmt.Println(fe.ErrorStack())
			}
		}
	}
	defer os.Exit(exitCode)
}
