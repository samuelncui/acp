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
	continueReport  = flag.String("c", "", "continue with previous report, for auto fill circumstances")
	noTarget        = flag.Bool("notarget", false, "do not have target, use as dir index tool")
	reportPath      = flag.String("report", "", "json report storage path")
	reportIndent    = flag.Bool("report-indent", false, "json report with indent")
	fromLinear      = flag.Bool("from-linear", false, "copy from linear device, such like tape drive")
	toLinear        = flag.Bool("to-linear", false, "copy to linear device, such like tape drive")
	autoFillDepth   = flag.Int("auto-fill-depth", 0, "auto fill the whole filesystem, split by specify filepath depth, and report will output left filepaths")

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
	if *continueReport != "" {
		f, err := os.Open(*continueReport)
		if err != nil {
			logrus.Fatalf("cannot open continue report file, %s", err)
		}

		r := new(acp.Report)
		if err := json.NewDecoder(f).Decode(r); err != nil {
			logrus.Fatalf("decode continue report file, %s", err)
		}

		for _, s := range r.NoSpaceSources {
			logrus.Infof("restore unfinished: base= '%s' relative_path= '%s'", s.Base, s.RelativePath)
			opts = append(opts, acp.AccurateSource(s.Base, s.RelativePath))
		}
	} else {
		opts = append(opts, acp.Source(sources...))
	}

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
	if *autoFillDepth != 0 {
		opts = append(opts, acp.WithAutoFill(true, *autoFillDepth))
	}

	c, err := acp.New(ctx, opts...)
	if err != nil {
		logrus.Fatalf("unexpected exit: %s", err)
	}

	defer func() {
		if *reportPath == "" {
			return
		}

		report := c.Report()
		r, err := os.Create(*reportPath)
		if err != nil {
			logrus.Warnf("open report fail, path= '%s', err= %w", *reportPath, err)
			logrus.Infof("report: %s", report)
			return
		}
		defer r.Close()

		var buf []byte
		if *reportIndent {
			buf, _ = json.MarshalIndent(report, "", "\t")
		} else {
			buf, _ = json.Marshal(report)
		}

		r.Write(buf)
	}()
	c.Wait()
}
