package common_test

import (
	"context"
	"testing"

	"github.com/sirupsen/logrus"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/isovalent/hubble-otel/common"
	"github.com/isovalent/hubble-otel/logconv"
	"github.com/isovalent/hubble-otel/receiver"
	"github.com/isovalent/hubble-otel/testutil"
)

const (
	hubbleAddress = "localhost:4245"
	logBufferSize = 2048
)

func BenchmarkAllModes(b *testing.B) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	fatal := make(chan error, 1)

	log := logrus.New()
	log.SetLevel(logrus.ErrorLevel)

	go testutil.RunMockHubble(ctx, log, "../testdata/2021-06-16-sample-flows-istio-gke", hubbleAddress, 100, nil, fatal)

	go func() {
		for err := range fatal {
			b.Errorf("fatal error in a goroutine: %v", err)
			cancel()
			return
		}
	}()

	testutil.WaitForServer(ctx, b.Logf, hubbleAddress)

	hubbleConn, err := grpc.DialContext(ctx, hubbleAddress, grpc.WithInsecure())
	if err != nil {
		b.Fatalf("failed to connect to Hubble server: %v", err)
	}

	defer hubbleConn.Close()

	for _, encoding := range common.EncodingFormats() {
		process := func() {
			flows := make(chan protoreflect.Message, logBufferSize)
			errs := make(chan error)

			go receiver.Run(ctx, hubbleConn, logconv.NewFlowConverter(encoding, false), flows, errs)
			for {
				select {
				case _ = <-flows: // drop
				case <-ctx.Done():
					return
				case err := <-errs:
					if testutil.IsEOF(err) {
						return
					}
					b.Fatal(err)
				}
			}
		}

		b.Run(encoding, func(b *testing.B) {
			for n := 0; n < b.N; n++ {
				process()
			}
		})
	}
}