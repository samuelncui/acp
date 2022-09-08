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
	notOverwrite = flag.Bool("n", false, "not overwrite exist file")
	noTarget     = flag.Bool("notarget", false, "do not have target, use as dir index tool")
	reportPath   = flag.String("report", "", "json report storage path")
	targetPaths  []string
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

	c, err := acp.New(
		ctx, acp.Source(sources...), acp.Target(targetPaths...),
		acp.Overwrite(!*notOverwrite), acp.WithProgressBar(true), acp.WithHash(true),
	)
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
