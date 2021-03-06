// Copyright 2017 The Cockroach Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License.

package log_test

import (
	"regexp"
	"testing"

	"golang.org/x/net/context"

	"github.com/cockroachdb/cockroach/pkg/base"
	"github.com/cockroachdb/cockroach/pkg/settings"
	"github.com/cockroachdb/cockroach/pkg/testutils/serverutils"
	"github.com/cockroachdb/cockroach/pkg/util/leaktest"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	raven "github.com/getsentry/raven-go"
)

// interceptingTransport is an implementation of raven.Transport that delegates
// calls to the Send method to the send function contained within.
type interceptingTransport struct {
	send func(url, authHeader string, packet *raven.Packet)
}

// Send implements the raven.Transport interface.
func (it interceptingTransport) Send(url, authHeader string, packet *raven.Packet) error {
	it.send(url, authHeader, packet)
	return nil
}

func TestCrashReportingPacket(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer raven.Close()

	ctx := context.Background()
	var packets []*raven.Packet

	// Temporarily enable all crash-reporting settings.
	defer settings.TestingSetBool(&log.DiagnosticsReportingEnabled, true)()
	defer log.TestingSetCrashReportingURL("https://ignored:ignored@ignored/ignored")()

	// Install a Transport that locally records packets rather than sending them
	// to Sentry over HTTP.
	defer func(transport raven.Transport) {
		raven.DefaultClient.Transport = transport
	}(raven.DefaultClient.Transport)
	raven.DefaultClient.Transport = interceptingTransport{
		send: func(_, _ string, packet *raven.Packet) {
			packets = append(packets, packet)
		},
	}

	expectPanic := func(name string) {
		if r := recover(); r == nil {
			t.Fatalf("'%s' failed to panic", name)
		}
	}

	log.SetupCrashReporter(ctx, "test")

	func() {
		defer expectPanic("before server start")
		defer log.RecoverAndReportPanic(ctx)
		panic("oh te noes!")
	}()

	func() {
		defer expectPanic("after server start")
		defer log.RecoverAndReportPanic(ctx)
		s, _, _ := serverutils.StartServer(t, base.TestServerArgs{})
		s.Stopper().Stop(ctx)
		panic("oh te noes!")
	}()

	expectations := []struct {
		serverID *regexp.Regexp
		tagCount int
	}{
		{regexp.MustCompile(`^$`), 5},
		{regexp.MustCompile(`^[a-z0-9]{8}-1$`), 8},
	}

	if e, a := len(expectations), len(packets); e != a {
		t.Fatalf("expected %d packets, but got %d", e, a)
	}

	for i := range expectations {
		if e, a := "<redacted>", packets[i].ServerName; e != a {
			t.Errorf("expected ServerName to be '<redacted>', but got '%s'", a)
		}

		tags := make(map[string]string, len(packets[i].Tags))
		for _, tag := range packets[i].Tags {
			tags[tag.Key] = tag.Value
		}

		if e, a := expectations[i].tagCount, len(tags); e != a {
			t.Errorf("%d: expected %d tags, but got %d", i, e, a)
		}

		if serverID := tags["server_id"]; !expectations[i].serverID.MatchString(serverID) {
			t.Errorf("%d: expected server_id '%s' to match %s", i, serverID, expectations[i].serverID)
		}
	}
}
