package main

import (
	"context"
	"encoding/json"
	"flag"
	"os"
	"os/signal"

	"github.com/abc950309/acp"
	"github.com/sirupsen/logrus"
)

var (
	withProgressBar = flag.Bool("p", true, "display progress bar")
	notOverwrite    = flag.Bool("n", false, "not overwrite exist file")
	noTarget        = flag.Bool("notarget", false, "do not have target, use as dir index tool")
	reportPath      = flag.String("report", "", "json report storage path")
	fromLinear      = flag.Bool("from-linear", false, "json report storage path")
	toLinear        = flag.Bool("to-linear", false, "json report storage path")

	targetPaths []string
)

func init() {
	flag.Func("target", "use target flag to give multi target path", func(s string) error {
		targetPaths = append(targetPaths, s)
		return nil
	})
}

func main() {
	ctx, cancel := context.WithCancel(context.Background())

	flag.Parse()
	sources := flag.Args()
	if len(sources) == 0 {
		logrus.Fatalf("cannot found source path")
	}

	if !*noTarget && len(targetPaths) == 0 {
		targetPaths = append(targetPaths, sources[len(sources)-1])
		sources = sources[:len(sources)-1]
	}
	if len(sources) == 0 {
		logrus.Fatalf("cannot found source path")
	}

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt)
	go func() {
		for sig := range signals {
			if sig != os.Interrupt {
				continue
			}
			cancel()
		}
	}()

	opts := make([]acp.Option, 0, 8)
	opts = append(opts, acp.Source(sources...))
	opts = append(opts, acp.Target(targetPaths...))
	opts = append(opts, acp.WithHash(*reportPath != ""))
	opts = append(opts, acp.Overwrite(!*notOverwrite))
	opts = append(opts, acp.WithProgressBar(*withProgressBar))

	if *fromLinear {
		opts = append(opts, acp.SetFromDevice(acp.LinearDevice(true)))
	}
	if *toLinear {
		opts = append(opts, acp.SetToDevice(acp.LinearDevice(true)))
	}

	c, err := acp.New(ctx, opts...)
	if err != nil {
		panic(err)
	}

	report := c.Wait()
	if *reportPath == "" {
		return
	}

	r, err := os.Create(*reportPath)
	if err != nil {
		logrus.Warnf("open report fail, path= '%s', err= %w", *reportPath, err)
		logrus.Infof("report: %s", report)
		return
	}
	defer r.Close()

	if err := json.NewEncoder(r).Encode(report); err != nil {
		logrus.Fatalf("export report fail, err= %s", err)
	}
}
