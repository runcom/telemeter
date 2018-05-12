package main

import (
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/prometheus/common/expfmt"
	"github.com/spf13/cobra"

	"github.com/smarterclayton/telemeter/pkg/authorizer/remote"
	"github.com/smarterclayton/telemeter/pkg/forwarder"
	telemeterhttp "github.com/smarterclayton/telemeter/pkg/http"
	"github.com/smarterclayton/telemeter/pkg/transform"
)

func main() {
	opt := &Options{
		Listen:     "localhost:9002",
		LimitBytes: 200 * 1024,
		Rules:      []string{`{__name__="openshift_build_info"}`},
	}
	cmd := &cobra.Command{
		Short: "Federate Prometheus via push",

		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return opt.Run()
		},
	}

	cmd.Flags().StringVar(&opt.Listen, "listen", opt.Listen, "A host:port to listen on for health and metrics.")
	cmd.Flags().StringVar(&opt.From, "from", opt.From, "The Prometheus server to federate from.")
	cmd.Flags().StringVar(&opt.FromToken, "from-token", opt.FromToken, "A bearer token to use when authenticating to the source Prometheus server.")
	cmd.Flags().StringVar(&opt.To, "to", opt.To, "A telemeter server endpoint to push metrics to.")
	cmd.Flags().StringVar(&opt.ToAuthorize, "to-auth", opt.ToAuthorize, "A telemeter server endpoint to exchange the bearer token for an access token.")
	cmd.Flags().StringVar(&opt.ToToken, "to-token", opt.ToToken, "A bearer token to use when authenticating to the destination telemeter server.")
	cmd.Flags().StringSliceVar(&opt.LabelFlag, "label", opt.LabelFlag, "Labels to add to each outgoing metric, in key=value form.")
	cmd.Flags().StringSliceVar(&opt.Rules, "match", opt.Rules, "Match rules to federate.")

	if err := cmd.Execute(); err != nil {
		os.Exit(1)
	}
}

type Options struct {
	Listen     string
	LimitBytes int64

	From        string
	To          string
	ToAuthorize string
	FromToken   string
	ToToken     string

	Rules []string

	LabelFlag []string
	Labels    map[string]string

	LabelRetriever transform.LabelRetriever
}

func (o *Options) Transforms() []transform.Interface {
	var transforms transform.All
	if len(o.Labels) > 0 || o.LabelRetriever != nil {
		transforms = append(transforms, transform.NewLabel(o.Labels, o.LabelRetriever))
	}
	transforms = append(transforms,
		transform.NewDropInvalidFederateSamples(time.Now().Add(-24*time.Hour)),
		transform.PackMetrics,
		transform.SortMetrics,
	)
	return []transform.Interface{transforms}
}

func (o *Options) MatchRules() []string {
	return o.Rules
}

func (o *Options) Run() error {
	if len(o.From) == 0 {
		return fmt.Errorf("you must specify a Prometheus server to federate from (e.g. http://localhost:9090)")
	}
	for _, flag := range o.LabelFlag {
		values := strings.SplitN(flag, "=", 2)
		if len(values) != 2 {
			return fmt.Errorf("--label must be of the form key=value: %s", flag)
		}
		if o.Labels == nil {
			o.Labels = make(map[string]string)
		}
		o.Labels[values[0]] = values[1]
	}

	from, err := url.Parse(o.From)
	if err != nil {
		return fmt.Errorf("--from is not a valid URL: %v", err)
	}
	from.Path = strings.TrimRight(from.Path, "/")
	if len(from.Path) == 0 {
		from.Path = "/federate"
	}

	var to, toAuthorize *url.URL
	if len(o.To) > 0 {
		to, err = url.Parse(o.To)
		if err != nil {
			return fmt.Errorf("--to is not a valid URL: %v", err)
		}
	}
	if len(o.ToAuthorize) > 0 {
		toAuthorize, err = url.Parse(o.ToAuthorize)
		if err != nil {
			return fmt.Errorf("--to-auth is not a valid URL: %v", err)
		}
	}

	fromClient := &http.Client{Transport: forwarder.DefaultTransport()}
	if len(o.FromToken) > 0 {
		fromClient.Transport = telemeterhttp.NewBearerRoundTripper(o.FromToken, fromClient.Transport)
	}
	toClient := &http.Client{Transport: forwarder.DefaultTransport()}
	if len(o.ToToken) > 0 {
		// exchange our token for a token from the authorize endpoint, which also gives us a
		// set of expected labels we must include
		rt := remote.NewServerRotatingRoundTripper(o.ToToken, toAuthorize, toClient.Transport)
		o.LabelRetriever = rt
		toClient.Transport = rt
	}

	worker := forwarder.New(*from, to, o)
	worker.ToClient = toClient
	worker.FromClient = fromClient
	worker.MaxBytes = o.LimitBytes

	go worker.Run()

	if len(o.Listen) > 0 {
		handlers := http.NewServeMux()
		telemeterhttp.AddDebug(handlers)
		telemeterhttp.AddHealth(handlers)
		telemeterhttp.AddMetrics(handlers)
		handlers.Handle("/federate", serveLastMetrics(worker))
		go func() {
			if err := http.ListenAndServe(o.Listen, handlers); err != nil && err != http.ErrServerClosed {
				log.Printf("error: server exited: %v", err)
				os.Exit(1)
			}
		}()
	}

	select {}
}

// serveLastMetrics retrieves the last set of metrics served
func serveLastMetrics(worker *forwarder.Worker) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != "GET" {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		mf := worker.LastMetrics()
		w.Header().Set("Content-Type", string(expfmt.FmtText))
		encoder := expfmt.NewEncoder(w, expfmt.FmtText)
		for _, family := range mf {
			if family == nil {
				continue
			}
			if err := encoder.Encode(family); err != nil {
				log.Printf("Unable to write metrics for family: %v", err)
				break
			}
		}
	})
}