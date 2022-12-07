package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"unsafe"

	"github.com/abc950309/acp"
	jsoniter "github.com/json-iterator/go"
	"github.com/modern-go/reflect2"
	"github.com/sirupsen/logrus"
)

var (
	withProgressBar = flag.Bool("p", true, "display progress bar")
	notOverwrite    = flag.Bool("n", false, "not overwrite exist file")
	// continueReport  = flag.String("c", "", "continue with previous report, for auto fill circumstances")
	noTarget     = flag.Bool("notarget", false, "do not have target, use as dir index tool")
	reportPath   = flag.String("report", "", "json report storage path")
	reportIndent = flag.Bool("report-indent", false, "json report with indent")
	fromLinear   = flag.Bool("from-linear", false, "copy from linear device, such like tape drive")
	toLinear     = flag.Bool("to-linear", false, "copy to linear device, such like tape drive")

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
	// if *continueReport != "" {
	// 	f, err := os.Open(*continueReport)
	// 	if err != nil {
	// 		logrus.Fatalf("cannot open continue report file, %s", err)
	// 	}

	// 	r := new(acp.Report)
	// 	if err := json.NewDecoder(f).Decode(r); err != nil {
	// 		logrus.Fatalf("decode continue report file, %s", err)
	// 	}

	// 	for _, s := range r.NoSpaceSources {
	// 		logrus.Infof("restore unfinished: base= '%s' relative_path= '%v'", s.Base, s.RelativePaths)
	// 		opts = append(opts, acp.AccurateSource(s.Base, s.RelativePaths...))
	// 	}
	// }

	opts = append(opts, acp.Target(targetPaths...))
	opts = append(opts, acp.WithHash(*reportPath != ""))
	opts = append(opts, acp.Overwrite(!*notOverwrite))

	if *withProgressBar {
		opts = append(opts, acp.WithProgressBar())
	}

	if *fromLinear {
		opts = append(opts, acp.SetFromDevice(acp.LinearDevice(true)))
	}
	if *toLinear {
		opts = append(opts, acp.SetToDevice(acp.LinearDevice(true)))
	}

	if *reportPath != "" {
		handler, getter := acp.NewReportGetter()
		opts = append(opts, acp.WithEventHandler(handler))
		defer func() {
			if *reportPath == "" {
				return
			}

			report := getter()
			r, err := os.Create(*reportPath)
			if err != nil {
				logrus.Warnf("open report fail, path= '%s', err= %w", *reportPath, err)
				logrus.Infof("report: %s", report)
				return
			}
			defer r.Close()

			var buf []byte
			if *reportIndent {
				buf, _ = reportJSON.MarshalIndent(report, "", "\t")
			} else {
				buf, _ = reportJSON.Marshal(report)
			}

			r.Write(buf)
		}()
	}

	c, err := acp.New(ctx, opts...)
	if err != nil {
		logrus.Fatalf("unexpected exit: %s", err)
	}

	c.Wait()
}

var (
	reportJSON jsoniter.API
)

type errValCoder struct{}

func (*errValCoder) IsEmpty(ptr unsafe.Pointer) bool {
	val := (*error)(ptr)
	return *val == nil
}

func (*errValCoder) Encode(ptr unsafe.Pointer, stream *jsoniter.Stream) {
	val := (*error)(ptr)
	stream.WriteString((*val).Error())
}

func (*errValCoder) Decode(ptr unsafe.Pointer, iter *jsoniter.Iterator) {
	val := (*error)(ptr)
	*val = fmt.Errorf(iter.ReadString())
}

func init() {
	reportJSON = jsoniter.Config{
		EscapeHTML:             true,
		SortMapKeys:            true,
		ValidateJsonRawMessage: true,
	}.Froze()

	var emptyErr error
	reportJSON.RegisterExtension(jsoniter.EncoderExtension{
		reflect2.TypeOf(emptyErr): &errValCoder{},
	})
	reportJSON.RegisterExtension(jsoniter.DecoderExtension{
		reflect2.TypeOf(emptyErr): &errValCoder{},
	})
}
