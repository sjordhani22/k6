/*
 *
 * k6 - a next-generation load testing tool
 * Copyright (C) 2017 Load Impact
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU Affero General Public License as
 * published by the Free Software Foundation, either version 3 of the
 * License, or (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU Affero General Public License for more details.
 *
 * You should have received a copy of the GNU Affero General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 *
 */
package ws

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/dop251/goja"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/guregu/null.v3"

	"go.k6.io/k6/js/common"
	httpModule "go.k6.io/k6/js/modules/k6/http"
	"go.k6.io/k6/lib"
	"go.k6.io/k6/lib/metrics"
	"go.k6.io/k6/lib/testutils/httpmultibin"

	"go.k6.io/k6/stats"
)

const statusProtocolSwitch = 101

func assertSessionMetricsEmitted(t *testing.T, sampleContainers []stats.SampleContainer, subprotocol, url string, status int, group string) {
	seenSessions := false
	seenSessionDuration := false
	seenConnecting := false

	for _, sampleContainer := range sampleContainers {
		for _, sample := range sampleContainer.GetSamples() {
			tags := sample.Tags.CloneTags()
			if tags["url"] == url {
				switch sample.Metric.Name {
				case metrics.WSConnectingName:
					seenConnecting = true
				case metrics.WSSessionDurationName:
					seenSessionDuration = true
				case metrics.WSSessionsName:
					seenSessions = true
				}

				assert.Equal(t, strconv.Itoa(status), tags["status"])
				assert.Equal(t, subprotocol, tags["subproto"])
				assert.Equal(t, group, tags["group"])
			}
		}
	}
	assert.True(t, seenConnecting, "url %s didn't emit Connecting", url)
	assert.True(t, seenSessions, "url %s didn't emit Sessions", url)
	assert.True(t, seenSessionDuration, "url %s didn't emit SessionDuration", url)
}

func assertMetricEmittedCount(t *testing.T, metricName string, sampleContainers []stats.SampleContainer, url string, count int) {
	t.Helper()
	actualCount := 0

	for _, sampleContainer := range sampleContainers {
		for _, sample := range sampleContainer.GetSamples() {
			surl, ok := sample.Tags.Get("url")
			assert.True(t, ok)
			if surl == url && sample.Metric.Name == metricName {
				actualCount++
			}
		}
	}
	assert.Equal(t, count, actualCount, "url %s emitted %s %d times, expected was %d times", url, metricName, actualCount, count)
}

type testState struct {
	ctxPtr  *context.Context
	rt      *goja.Runtime
	tb      *httpmultibin.HTTPMultiBin
	state   *lib.State
	samples chan stats.SampleContainer
}

func newTestState(t testing.TB) testState {
	tb := httpmultibin.NewHTTPMultiBin(t)

	root, err := lib.NewGroup("", nil)
	require.NoError(t, err)

	rt := goja.New()
	rt.SetFieldNameMapper(common.FieldNameMapper{})

	samples := make(chan stats.SampleContainer, 1000)

	state := &lib.State{
		Group:  root,
		Dialer: tb.Dialer,
		Options: lib.Options{
			SystemTags: stats.NewSystemTagSet(
				stats.TagURL,
				stats.TagProto,
				stats.TagStatus,
				stats.TagSubproto,
			),
			UserAgent: null.StringFrom("TestUserAgent"),
		},
		Samples:        samples,
		TLSConfig:      tb.TLSClientConfig,
		BuiltinMetrics: metrics.RegisterBuiltinMetrics(metrics.NewRegistry()),
	}

	ctx := new(context.Context)
	*ctx = lib.WithState(tb.Context, state)
	*ctx = common.WithRuntime(*ctx, rt)
	err = rt.Set("ws", common.Bind(rt, New(), ctx))
	assert.NoError(t, err)

	return testState{
		ctxPtr:  ctx,
		rt:      rt,
		tb:      tb,
		state:   state,
		samples: samples,
	}
}

func TestSession(t *testing.T) {
	// TODO: split and paralelize tests
	t.Parallel()
	tb := httpmultibin.NewHTTPMultiBin(t)
	sr := tb.Replacer.Replace

	root, err := lib.NewGroup("", nil)
	assert.NoError(t, err)

	rt := goja.New()
	rt.SetFieldNameMapper(common.FieldNameMapper{})
	samples := make(chan stats.SampleContainer, 1000)
	state := &lib.State{
		Group:  root,
		Dialer: tb.Dialer,
		Options: lib.Options{
			SystemTags: stats.NewSystemTagSet(
				stats.TagURL,
				stats.TagProto,
				stats.TagStatus,
				stats.TagSubproto,
			),
		},
		Samples:        samples,
		TLSConfig:      tb.TLSClientConfig,
		BuiltinMetrics: metrics.RegisterBuiltinMetrics(metrics.NewRegistry()),
	}

	ctx := context.Background()
	ctx = lib.WithState(ctx, state)
	ctx = common.WithRuntime(ctx, rt)

	rt.Set("ws", common.Bind(rt, New(), &ctx))

	t.Run("connect_ws", func(t *testing.T) {
		_, err := rt.RunString(sr(`
		var res = ws.connect("WSBIN_URL/ws-echo", function(socket){
			socket.close()
		});
		if (res.status != 101) { throw new Error("connection failed with status: " + res.status); }
		`))
		assert.NoError(t, err)
	})
	assertSessionMetricsEmitted(t, stats.GetBufferedSamples(samples), "", sr("WSBIN_URL/ws-echo"), statusProtocolSwitch, "")

	t.Run("connect_wss", func(t *testing.T) {
		_, err := rt.RunString(sr(`
		var res = ws.connect("WSSBIN_URL/ws-echo", function(socket){
			socket.close()
		});
		if (res.status != 101) { throw new Error("TLS connection failed with status: " + res.status); }
		`))
		assert.NoError(t, err)
	})
	assertSessionMetricsEmitted(t, stats.GetBufferedSamples(samples), "", sr("WSSBIN_URL/ws-echo"), statusProtocolSwitch, "")

	t.Run("open", func(t *testing.T) {
		_, err := rt.RunString(sr(`
		var opened = false;
		var res = ws.connect("WSBIN_URL/ws-echo", function(socket){
			socket.on("open", function() {
				opened = true;
				socket.close()
			})
		});
		if (!opened) { throw new Error ("open event not fired"); }
		`))
		assert.NoError(t, err)
	})
	assertSessionMetricsEmitted(t, stats.GetBufferedSamples(samples), "", sr("WSBIN_URL/ws-echo"), statusProtocolSwitch, "")

	t.Run("send_receive", func(t *testing.T) {
		_, err := rt.RunString(sr(`
		var res = ws.connect("WSBIN_URL/ws-echo", function(socket){
			socket.on("open", function() {
				socket.send("test")
			})
			socket.on("message", function (data) {
				if (!data=="test") {
					throw new Error ("echo'd data doesn't match our message!");
				}
				socket.close()
			});
		});
		`))
		assert.NoError(t, err)
	})

	samplesBuf := stats.GetBufferedSamples(samples)
	assertSessionMetricsEmitted(t, samplesBuf, "", sr("WSBIN_URL/ws-echo"), statusProtocolSwitch, "")
	assertMetricEmittedCount(t, metrics.WSMessagesSentName, samplesBuf, sr("WSBIN_URL/ws-echo"), 1)
	assertMetricEmittedCount(t, metrics.WSMessagesReceivedName, samplesBuf, sr("WSBIN_URL/ws-echo"), 1)

	t.Run("interval", func(t *testing.T) {
		_, err := rt.RunString(sr(`
		var counter = 0;
		var res = ws.connect("WSBIN_URL/ws-echo", function(socket){
			socket.setInterval(function () {
				counter += 1;
				if (counter > 2) { socket.close(); }
			}, 100);
		});
		if (counter < 3) {throw new Error ("setInterval should have been called at least 3 times, counter=" + counter);}
		`))
		assert.NoError(t, err)
	})
	assertSessionMetricsEmitted(t, stats.GetBufferedSamples(samples), "", sr("WSBIN_URL/ws-echo"), statusProtocolSwitch, "")
	t.Run("bad interval", func(t *testing.T) {
		_, err := rt.RunString(sr(`
		var counter = 0;
		var res = ws.connect("WSBIN_URL/ws-echo", function(socket){
			socket.setInterval(function () {
				counter += 1;
				if (counter > 2) { socket.close(); }
			}, -1.23);
		});
		`))
		require.Error(t, err)
		require.Contains(t, err.Error(), "setInterval requires a >0 timeout parameter, received -1.23 ")
	})

	t.Run("timeout", func(t *testing.T) {
		_, err := rt.RunString(sr(`
		var start = new Date().getTime();
		var ellapsed = new Date().getTime() - start;
		var res = ws.connect("WSBIN_URL/ws-echo", function(socket){
			socket.setTimeout(function () {
				ellapsed = new Date().getTime() - start;
				socket.close();
			}, 500);
		});
		if (ellapsed > 3000 || ellapsed < 500) {
			throw new Error ("setTimeout occurred after " + ellapsed + "ms, expected 500<T<3000");
		}
		`))
		assert.NoError(t, err)
	})

	t.Run("bad timeout", func(t *testing.T) {
		_, err := rt.RunString(sr(`
		var start = new Date().getTime();
		var ellapsed = new Date().getTime() - start;
		var res = ws.connect("WSBIN_URL/ws-echo", function(socket){
			socket.setTimeout(function () {
				ellapsed = new Date().getTime() - start;
				socket.close();
			}, 0);
		});
		`))
		require.Error(t, err)
		require.Contains(t, err.Error(), "setTimeout requires a >0 timeout parameter, received 0.00 ")
	})
	assertSessionMetricsEmitted(t, stats.GetBufferedSamples(samples), "", sr("WSBIN_URL/ws-echo"), statusProtocolSwitch, "")

	t.Run("ping", func(t *testing.T) {
		_, err := rt.RunString(sr(`
		var pongReceived = false;
		var res = ws.connect("WSBIN_URL/ws-echo", function(socket){
			socket.on("open", function(data) {
				socket.ping();
			});
			socket.on("pong", function() {
				pongReceived = true;
				socket.close();
			});
			socket.setTimeout(function (){socket.close();}, 3000);
		});
		if (!pongReceived) {
			throw new Error ("sent ping but didn't get pong back");
		}
		`))
		assert.NoError(t, err)
	})

	samplesBuf = stats.GetBufferedSamples(samples)
	assertSessionMetricsEmitted(t, samplesBuf, "", sr("WSBIN_URL/ws-echo"), statusProtocolSwitch, "")
	assertMetricEmittedCount(t, metrics.WSPingName, samplesBuf, sr("WSBIN_URL/ws-echo"), 1)

	t.Run("multiple_handlers", func(t *testing.T) {
		_, err := rt.RunString(sr(`
		var pongReceived = false;
		var otherPongReceived = false;

		var res = ws.connect("WSBIN_URL/ws-echo", function(socket){
			socket.on("open", function(data) {
				socket.ping();
			});
			socket.on("pong", function() {
				pongReceived = true;
				if (otherPongReceived) {
					socket.close();
				}
			});
			socket.on("pong", function() {
				otherPongReceived = true;
				if (pongReceived) {
					socket.close();
				}
			});
			socket.setTimeout(function (){socket.close();}, 3000);
		});
		if (!pongReceived || !otherPongReceived) {
			throw new Error ("sent ping but didn't get pong back");
		}
		`))
		assert.NoError(t, err)
	})

	samplesBuf = stats.GetBufferedSamples(samples)
	assertSessionMetricsEmitted(t, samplesBuf, "", sr("WSBIN_URL/ws-echo"), statusProtocolSwitch, "")
	assertMetricEmittedCount(t, metrics.WSPingName, samplesBuf, sr("WSBIN_URL/ws-echo"), 1)

	t.Run("client_close", func(t *testing.T) {
		_, err := rt.RunString(sr(`
		var closed = false;
		var res = ws.connect("WSBIN_URL/ws-echo", function(socket){
			socket.on("open", function() {
							socket.close()
			})
			socket.on("close", function() {
							closed = true;
			})
		});
		if (!closed) { throw new Error ("close event not fired"); }
		`))
		assert.NoError(t, err)
	})
	assertSessionMetricsEmitted(t, stats.GetBufferedSamples(samples), "", sr("WSBIN_URL/ws-echo"), statusProtocolSwitch, "")

	serverCloseTests := []struct {
		name     string
		endpoint string
	}{
		{"server_close_ok", "/ws-echo"},
		// Ensure we correctly handle invalid WS server
		// implementations that close the connection prematurely
		// without sending a close control frame first.
		{"server_close_invalid", "/ws-close-invalid"},
	}

	for _, tc := range serverCloseTests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			_, err := rt.RunString(sr(fmt.Sprintf(`
			var closed = false;
			var res = ws.connect("WSBIN_URL%s", function(socket){
				socket.on("open", function() {
					socket.send("test");
				})
				socket.on("close", function() {
					closed = true;
				})
			});
			if (!closed) { throw new Error ("close event not fired"); }
			`, tc.endpoint)))
			assert.NoError(t, err)
		})
	}

	t.Run("multi_message", func(t *testing.T) {
		t.Parallel()

		tb.Mux.HandleFunc("/ws-echo-multi", http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			conn, err := (&websocket.Upgrader{}).Upgrade(w, req, w.Header())
			if err != nil {
				return
			}

			for {
				messageType, r, e := conn.NextReader()
				if e != nil {
					return
				}
				var wc io.WriteCloser
				wc, err = conn.NextWriter(messageType)
				if err != nil {
					return
				}
				if _, err = io.Copy(wc, r); err != nil {
					return
				}
				if err = wc.Close(); err != nil {
					return
				}
			}
		}))

		t.Run("send_receive_multiple_ws", func(t *testing.T) {
			_, err := rt.RunString(sr(`
			var msg1 = "test1"
			var msg2 = "test2"
			var msg3 = "test3"
			var allMsgsRecvd = false
			var res = ws.connect("WSBIN_URL/ws-echo-multi", (socket) => {
				socket.on("open", () => {
					socket.send(msg1)
				})
				socket.on("message", (data) => {
					if (data == msg1){
						socket.send(msg2)
					}
					if (data == msg2){
						socket.send(msg3)
					}
					if (data == msg3){
						allMsgsRecvd = true
						socket.close()
					}
				});
			});

			if (!allMsgsRecvd) {
				throw new Error ("messages 1,2,3 in sequence, was not received from server");
			}
			`))
			assert.NoError(t, err)
		})

		samplesBuf = stats.GetBufferedSamples(samples)
		assertSessionMetricsEmitted(t, samplesBuf, "", sr("WSBIN_URL/ws-echo-multi"), statusProtocolSwitch, "")
		assertMetricEmittedCount(t, metrics.WSMessagesSentName, samplesBuf, sr("WSBIN_URL/ws-echo-multi"), 3)
		assertMetricEmittedCount(t, metrics.WSMessagesReceivedName, samplesBuf, sr("WSBIN_URL/ws-echo-multi"), 3)

		t.Run("send_receive_multiple_wss", func(t *testing.T) {
			_, err := rt.RunString(sr(`
			var msg1 = "test1"
			var msg2 = "test2"
			var secondMsgReceived = false
			var res = ws.connect("WSSBIN_URL/ws-echo-multi", (socket) => {
				socket.on("open", () => {
					socket.send(msg1)
				})
				socket.on("message", (data) => {
					if (data == msg1){
						socket.send(msg2)
					}
					if (data == msg2){
						secondMsgReceived = true
						socket.close()
					}
				});
			});

			if (!secondMsgReceived) {
				throw new Error ("second test message was not received from server!");
			}
			`))
			assert.NoError(t, err)
		})

		samplesBuf = stats.GetBufferedSamples(samples)
		assertSessionMetricsEmitted(t, samplesBuf, "", sr("WSSBIN_URL/ws-echo-multi"), statusProtocolSwitch, "")
		assertMetricEmittedCount(t, metrics.WSMessagesSentName, samplesBuf, sr("WSSBIN_URL/ws-echo-multi"), 2)
		assertMetricEmittedCount(t, metrics.WSMessagesReceivedName, samplesBuf, sr("WSSBIN_URL/ws-echo-multi"), 2)

		t.Run("send_receive_text_binary", func(t *testing.T) {
			_, err := rt.RunString(sr(`
			var msg1 = "test1"
			var msg2 = new Uint8Array([116, 101, 115, 116, 50]); // 'test2'
			var secondMsgReceived = false
			var res = ws.connect("WSBIN_URL/ws-echo-multi", (socket) => {
				socket.on("open", () => {
					socket.send(msg1)
				})
				socket.on("message", (data) => {
					if (data == msg1){
						socket.sendBinary(msg2.buffer)
					}
				});
				socket.on("binaryMessage", (data) => {
					let data2 = new Uint8Array(data)
					if(JSON.stringify(msg2) == JSON.stringify(data2)){
						secondMsgReceived = true
					}
					socket.close()
				})
			});

			if (!secondMsgReceived) {
				throw new Error ("second test message was not received from server!");
			}
			`))
			assert.NoError(t, err)
		})

		samplesBuf = stats.GetBufferedSamples(samples)
		assertSessionMetricsEmitted(t, samplesBuf, "", sr("WSBIN_URL/ws-echo-multi"), statusProtocolSwitch, "")
		assertMetricEmittedCount(t, metrics.WSMessagesSentName, samplesBuf, sr("WSBIN_URL/ws-echo-multi"), 2)
		assertMetricEmittedCount(t, metrics.WSMessagesReceivedName, samplesBuf, sr("WSBIN_URL/ws-echo-multi"), 2)
	})
}

func TestSocketSendBinary(t *testing.T) { //nolint: tparallel
	t.Parallel()
	tb := httpmultibin.NewHTTPMultiBin(t)
	sr := tb.Replacer.Replace

	root, err := lib.NewGroup("", nil)
	assert.NoError(t, err)

	rt := goja.New()
	rt.SetFieldNameMapper(common.FieldNameMapper{})
	samples := make(chan stats.SampleContainer, 1000)
	state := &lib.State{ //nolint: exhaustivestruct
		Group:  root,
		Dialer: tb.Dialer,
		Options: lib.Options{ //nolint: exhaustivestruct
			SystemTags: stats.NewSystemTagSet(
				stats.TagURL,
				stats.TagProto,
				stats.TagStatus,
				stats.TagSubproto,
			),
		},
		Samples:        samples,
		TLSConfig:      tb.TLSClientConfig,
		BuiltinMetrics: metrics.RegisterBuiltinMetrics(metrics.NewRegistry()),
	}

	ctx := context.Background()
	ctx = lib.WithState(ctx, state)
	ctx = common.WithRuntime(ctx, rt)

	err = rt.Set("ws", common.Bind(rt, New(), &ctx))
	assert.NoError(t, err)

	t.Run("ok", func(t *testing.T) {
		_, err = rt.RunString(sr(`
		var gotMsg = false;
		var res = ws.connect('WSBIN_URL/ws-echo', function(socket){
			var data = new Uint8Array([104, 101, 108, 108, 111]); // 'hello'

			socket.on('open', function() {
				socket.sendBinary(data.buffer);
			})
			socket.on('binaryMessage', function(msg) {
				gotMsg = true;
				let decText = String.fromCharCode.apply(null, new Uint8Array(msg));
				decText = decodeURIComponent(escape(decText));
				if (decText !== 'hello') {
					throw new Error('received unexpected binary message: ' + decText);
				}
				socket.close()
			});
		});
		if (!gotMsg) {
			throw new Error("the 'binaryMessage' handler wasn't called")
		}
		`))
		assert.NoError(t, err)
	})

	errTestCases := []struct {
		in, expErrType string
	}{
		{"", ""},
		{"undefined", "undefined"},
		{"null", "null"},
		{"true", "Boolean"},
		{"1", "Number"},
		{"3.14", "Number"},
		{"'str'", "String"},
		{"[1, 2, 3]", "Array"},
		{"new Uint8Array([1, 2, 3])", "Object"},
		{"Symbol('a')", "Symbol"},
		{"function() {}", "Function"},
	}

	for _, tc := range errTestCases { //nolint: paralleltest
		tc := tc
		t.Run(fmt.Sprintf("err_%s", tc.expErrType), func(t *testing.T) {
			_, err = rt.RunString(fmt.Sprintf(sr(`
			var res = ws.connect('WSBIN_URL/ws-echo', function(socket){
				socket.on('open', function() {
					socket.sendBinary(%s);
				})
			});
		`), tc.in))
			require.Error(t, err)
			if tc.in == "" {
				assert.Contains(t, err.Error(), "missing argument, expected ArrayBuffer")
			} else {
				assert.Contains(t, err.Error(), fmt.Sprintf("expected ArrayBuffer as argument, received: %s", tc.expErrType))
			}
		})
	}
}

func TestErrors(t *testing.T) {
	t.Parallel()
	tb := httpmultibin.NewHTTPMultiBin(t)
	sr := tb.Replacer.Replace

	root, err := lib.NewGroup("", nil)
	assert.NoError(t, err)

	rt := goja.New()
	rt.SetFieldNameMapper(common.FieldNameMapper{})
	samples := make(chan stats.SampleContainer, 1000)
	state := &lib.State{
		Group:  root,
		Dialer: tb.Dialer,
		Options: lib.Options{
			SystemTags: &stats.DefaultSystemTagSet,
		},
		Samples:        samples,
		BuiltinMetrics: metrics.RegisterBuiltinMetrics(metrics.NewRegistry()),
	}

	ctx := context.Background()
	ctx = lib.WithState(ctx, state)
	ctx = common.WithRuntime(ctx, rt)

	rt.Set("ws", common.Bind(rt, New(), &ctx))

	t.Run("invalid_url", func(t *testing.T) {
		_, err := rt.RunString(`
		var res = ws.connect("INVALID", function(socket){
			socket.on("open", function() {
				socket.close();
			});
		});
		`)
		assert.Error(t, err)
	})

	t.Run("invalid_url_message_panic", func(t *testing.T) {
		// Attempting to send a message to a non-existent socket shouldn't panic
		_, err := rt.RunString(`
		var res = ws.connect("INVALID", function(socket){
			socket.send("new message");
		});
		`)
		assert.Error(t, err)
	})

	t.Run("error_in_setup", func(t *testing.T) {
		_, err := rt.RunString(sr(`
		var res = ws.connect("WSBIN_URL/ws-echo-invalid", function(socket){
			throw new Error("error in setup");
		});
		`))
		assert.Error(t, err)
	})

	t.Run("send_after_close", func(t *testing.T) {
		_, err := rt.RunString(sr(`
		var hasError = false;
		var res = ws.connect("WSBIN_URL/ws-echo-invalid", function(socket){
			socket.on("open", function() {
				socket.close();
				socket.send("test");
			});

			socket.on("error", function(errorEvent) {
				hasError = true;
			});
		});
		if (!hasError) {
			throw new Error ("no error emitted for send after close");
		}
		`))
		assert.NoError(t, err)
		assertSessionMetricsEmitted(t, stats.GetBufferedSamples(samples), "", sr("WSBIN_URL/ws-echo-invalid"), statusProtocolSwitch, "")
	})

	t.Run("error on close", func(t *testing.T) {
		_, err := rt.RunString(sr(`
		var closed = false;
		var res = ws.connect("WSBIN_URL/ws-close", function(socket){
			socket.on('open', function open() {
				socket.setInterval(function timeout() {
				  socket.ping();
				}, 1000);
			});

			socket.on("ping", function() {
				socket.close();
			});

			socket.on("error", function(errorEvent) {
				if (errorEvent == null) {
					throw new Error(JSON.stringify(errorEvent));
				}
				if (!closed) {
					closed = true;
				    socket.close();
				}
			});
		});
		`))
		assert.NoError(t, err)
		assertSessionMetricsEmitted(t, stats.GetBufferedSamples(samples), "", sr("WSBIN_URL/ws-close"), statusProtocolSwitch, "")
	})
}

func TestSystemTags(t *testing.T) {
	tb := httpmultibin.NewHTTPMultiBin(t)

	sr := tb.Replacer.Replace

	root, err := lib.NewGroup("", nil)
	assert.NoError(t, err)

	rt := goja.New()
	rt.SetFieldNameMapper(common.FieldNameMapper{})

	// TODO: test for actual tag values after removing the dependency on the
	// external service demos.kaazing.com (https://github.com/k6io/k6/issues/537)
	testedSystemTags := []string{"group", "status", "subproto", "url", "ip"}

	samples := make(chan stats.SampleContainer, 1000)
	state := &lib.State{
		Group:          root,
		Dialer:         tb.Dialer,
		Options:        lib.Options{SystemTags: stats.ToSystemTagSet(testedSystemTags)},
		Samples:        samples,
		TLSConfig:      tb.TLSClientConfig,
		BuiltinMetrics: metrics.RegisterBuiltinMetrics(metrics.NewRegistry()),
	}

	ctx := context.Background()
	ctx = lib.WithState(ctx, state)
	ctx = common.WithRuntime(ctx, rt)

	rt.Set("ws", common.Bind(rt, New(), &ctx))

	for _, expectedTag := range testedSystemTags {
		expectedTag := expectedTag
		t.Run("only "+expectedTag, func(t *testing.T) {
			state.Options.SystemTags = stats.ToSystemTagSet([]string{expectedTag})
			_, err := rt.RunString(sr(`
			var res = ws.connect("WSBIN_URL/ws-echo", function(socket){
				socket.on("open", function() {
					socket.send("test")
				})
				socket.on("message", function (data){
					if (!data=="test") {
						throw new Error ("echo'd data doesn't match our message!");
					}
					socket.close()
				});
			});
			`))
			assert.NoError(t, err)

			for _, sampleContainer := range stats.GetBufferedSamples(samples) {
				for _, sample := range sampleContainer.GetSamples() {
					for emittedTag := range sample.Tags.CloneTags() {
						assert.Equal(t, expectedTag, emittedTag)
					}
				}
			}
		})
	}
}

func TestTLSConfig(t *testing.T) {
	root, err := lib.NewGroup("", nil)
	assert.NoError(t, err)

	tb := httpmultibin.NewHTTPMultiBin(t)

	sr := tb.Replacer.Replace

	rt := goja.New()
	rt.SetFieldNameMapper(common.FieldNameMapper{})
	samples := make(chan stats.SampleContainer, 1000)
	state := &lib.State{
		Group:  root,
		Dialer: tb.Dialer,
		Options: lib.Options{
			SystemTags: stats.NewSystemTagSet(
				stats.TagURL,
				stats.TagProto,
				stats.TagStatus,
				stats.TagSubproto,
				stats.TagIP,
			),
		},
		Samples:        samples,
		BuiltinMetrics: metrics.RegisterBuiltinMetrics(metrics.NewRegistry()),
	}

	ctx := context.Background()
	ctx = lib.WithState(ctx, state)
	ctx = common.WithRuntime(ctx, rt)

	rt.Set("ws", common.Bind(rt, New(), &ctx))

	t.Run("insecure skip verify", func(t *testing.T) {
		state.TLSConfig = &tls.Config{
			InsecureSkipVerify: true,
		}

		_, err := rt.RunString(sr(`
		var res = ws.connect("WSSBIN_URL/ws-close", function(socket){
			socket.close()
		});
		if (res.status != 101) { throw new Error("TLS connection failed with status: " + res.status); }
		`))
		assert.NoError(t, err)
	})
	assertSessionMetricsEmitted(t, stats.GetBufferedSamples(samples), "", sr("WSSBIN_URL/ws-close"), statusProtocolSwitch, "")

	t.Run("custom certificates", func(t *testing.T) {
		state.TLSConfig = tb.TLSClientConfig

		_, err := rt.RunString(sr(`
			var res = ws.connect("WSSBIN_URL/ws-close", function(socket){
				socket.close()
			});
			if (res.status != 101) {
				throw new Error("TLS connection failed with status: " + res.status);
			}
		`))
		assert.NoError(t, err)
	})
	assertSessionMetricsEmitted(t, stats.GetBufferedSamples(samples), "", sr("WSSBIN_URL/ws-close"), statusProtocolSwitch, "")
}

func TestReadPump(t *testing.T) {
	var closeCode int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, w.Header())
		assert.NoError(t, err)
		closeMsg := websocket.FormatCloseMessage(closeCode, "")
		_ = conn.WriteControl(websocket.CloseMessage, closeMsg, time.Now().Add(time.Second))
	}))
	defer srv.Close()

	closeCodes := []int{websocket.CloseNormalClosure, websocket.CloseGoingAway, websocket.CloseInternalServerErr}

	numAsserts := 0
	srvURL := "ws://" + srv.Listener.Addr().String()

	// Ensure readPump returns the response close code sent by the server
	for _, code := range closeCodes {
		code := code
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			closeCode = code
			conn, resp, err := websocket.DefaultDialer.Dial(srvURL, nil)
			assert.NoError(t, err)
			defer func() {
				_ = resp.Body.Close()
				_ = conn.Close()
			}()

			msgChan := make(chan *message)
			errChan := make(chan error)
			closeChan := make(chan int)
			s := &Socket{conn: conn}
			go s.readPump(msgChan, errChan, closeChan)

		readChans:
			for {
				select {
				case responseCode := <-closeChan:
					assert.Equal(t, code, responseCode)
					numAsserts++
					break readChans
				case <-errChan:
					continue
				case <-time.After(time.Second):
					t.Errorf("Read timed out")
					break readChans
				}
			}
		})
	}

	// Ensure all close code asserts passed
	assert.Equal(t, numAsserts, len(closeCodes))
}

func TestUserAgent(t *testing.T) {
	t.Parallel()
	tb := httpmultibin.NewHTTPMultiBin(t)
	sr := tb.Replacer.Replace

	tb.Mux.HandleFunc("/ws-echo-useragent", http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		// Echo back User-Agent header if it exists
		responseHeaders := w.Header().Clone()
		if ua := req.Header.Get("User-Agent"); ua != "" {
			responseHeaders.Add("Echo-User-Agent", req.Header.Get("User-Agent"))
		}

		conn, err := (&websocket.Upgrader{}).Upgrade(w, req, responseHeaders)
		if err != nil {
			t.Fatalf("/ws-echo-useragent cannot upgrade request: %v", err)
			return
		}

		err = conn.Close()
		if err != nil {
			t.Logf("error while closing connection in /ws-echo-useragent: %v", err)
			return
		}
	}))

	root, err := lib.NewGroup("", nil)
	assert.NoError(t, err)

	rt := goja.New()
	rt.SetFieldNameMapper(common.FieldNameMapper{})
	samples := make(chan stats.SampleContainer, 1000)
	state := &lib.State{
		Group:  root,
		Dialer: tb.Dialer,
		Options: lib.Options{
			SystemTags: stats.NewSystemTagSet(
				stats.TagURL,
				stats.TagProto,
				stats.TagStatus,
				stats.TagSubproto,
			),
			UserAgent: null.StringFrom("TestUserAgent"),
		},
		Samples:        samples,
		TLSConfig:      tb.TLSClientConfig,
		BuiltinMetrics: metrics.RegisterBuiltinMetrics(metrics.NewRegistry()),
	}

	ctx := lib.WithState(context.Background(), state)
	ctx = common.WithRuntime(ctx, rt)

	err = rt.Set("ws", common.Bind(rt, New(), &ctx))
	assert.NoError(t, err)

	// websocket handler should echo back User-Agent as Echo-User-Agent for this test to work
	_, err = rt.RunString(sr(`
		var res = ws.connect("WSBIN_URL/ws-echo-useragent", function(socket){
			socket.close()
		})
		var userAgent = res.headers["Echo-User-Agent"];
		if (userAgent == undefined) {
			throw new Error("user agent is not echoed back by test server");
		}
		if (userAgent != "TestUserAgent") {
			throw new Error("incorrect user agent: " + userAgent);
		}
		`))
	assert.NoError(t, err)

	assertSessionMetricsEmitted(t, stats.GetBufferedSamples(samples), "", sr("WSBIN_URL/ws-echo-useragent"), statusProtocolSwitch, "")
}

func TestCompression(t *testing.T) {
	t.Parallel()

	t.Run("session", func(t *testing.T) {
		t.Parallel()
		const text string = `Lorem ipsum dolor sit amet, consectetur adipiscing elit. Maecenas sed pharetra sapien. Nunc laoreet molestie ante ac gravida. Etiam interdum dui viverra posuere egestas. Pellentesque at dolor tristique, mattis turpis eget, commodo purus. Nunc orci aliquam.`

		ts := newTestState(t)
		sr := ts.tb.Replacer.Replace
		ts.tb.Mux.HandleFunc("/ws-compression", http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			upgrader := websocket.Upgrader{
				EnableCompression: true,
				ReadBufferSize:    1024,
				WriteBufferSize:   1024,
			}

			conn, e := upgrader.Upgrade(w, req, w.Header())
			if e != nil {
				t.Fatalf("/ws-compression cannot upgrade request: %v", e)
				return
			}

			// send a message and exit
			if e = conn.WriteMessage(websocket.TextMessage, []byte(text)); e != nil {
				t.Logf("error while sending message in /ws-compression: %v", e)
				return
			}

			e = conn.Close()
			if e != nil {
				t.Logf("error while closing connection in /ws-compression: %v", e)
				return
			}
		}))

		_, err := ts.rt.RunString(sr(`
		// if client supports compression, it has to send the header 
		// 'Sec-Websocket-Extensions:permessage-deflate; server_no_context_takeover; client_no_context_takeover' to server.
		// if compression is negotiated successfully, server will reply with header 
		// 'Sec-Websocket-Extensions:permessage-deflate; server_no_context_takeover; client_no_context_takeover'

		var params = {
			"compression": "deflate"
		}
		var res = ws.connect("WSBIN_URL/ws-compression", params, function(socket){
			socket.on('message', (data) => {
				if(data != "` + text + `"){
					throw new Error("wrong message received from server: ", data)
				}
				socket.close()
			})
		});

		var wsExtensions = res.headers["Sec-Websocket-Extensions"].split(';').map(e => e.trim())
		if (!(wsExtensions.includes("permessage-deflate") && wsExtensions.includes("server_no_context_takeover") && wsExtensions.includes("client_no_context_takeover"))){
			throw new Error("websocket compression negotiation failed");
		}
		`))

		assert.NoError(t, err)
		assertSessionMetricsEmitted(t, stats.GetBufferedSamples(ts.samples), "", sr("WSBIN_URL/ws-compression"), statusProtocolSwitch, "")
	})

	t.Run("params", func(t *testing.T) {
		t.Parallel()
		testCases := []struct {
			compression   string
			expectedError string
		}{
			{compression: ""},
			{compression: "  "},
			{compression: "deflate"},
			{compression: "deflate "},
			{
				compression:   "gzip",
				expectedError: `unsupported compression algorithm 'gzip', supported algorithm is 'deflate'`,
			},
			{
				compression:   "deflate, gzip",
				expectedError: `unsupported compression algorithm 'deflate, gzip', supported algorithm is 'deflate'`,
			},
			{
				compression:   "deflate, deflate",
				expectedError: `unsupported compression algorithm 'deflate, deflate', supported algorithm is 'deflate'`,
			},
			{
				compression:   "deflate, ",
				expectedError: `unsupported compression algorithm 'deflate,', supported algorithm is 'deflate'`,
			},
		}

		for _, testCase := range testCases {
			testCase := testCase
			t.Run(testCase.compression, func(t *testing.T) {
				t.Parallel()
				ts := newTestState(t)
				sr := ts.tb.Replacer.Replace
				ts.tb.Mux.HandleFunc("/ws-compression-param", http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
					upgrader := websocket.Upgrader{
						EnableCompression: true,
						ReadBufferSize:    1024,
						WriteBufferSize:   1024,
					}

					conn, e := upgrader.Upgrade(w, req, w.Header())
					if e != nil {
						t.Fatalf("/ws-compression-param cannot upgrade request: %v", e)
						return
					}

					e = conn.Close()
					if e != nil {
						t.Logf("error while closing connection in /ws-compression-param: %v", e)
						return
					}
				}))

				_, err := ts.rt.RunString(sr(`
					var res = ws.connect("WSBIN_URL/ws-compression-param", {"compression":"` + testCase.compression + `"}, function(socket){
						socket.close()
					});
				`))

				if testCase.expectedError == "" {
					require.NoError(t, err)
				} else {
					require.Error(t, err)
					require.Contains(t, err.Error(), testCase.expectedError)
				}
			})
		}
	})
}

func clearSamples(tb *httpmultibin.HTTPMultiBin, samples chan stats.SampleContainer) {
	ctxDone := tb.Context.Done()
	for {
		select {
		case <-samples:
		case <-ctxDone:
			return
		}
	}
}

func BenchmarkCompression(b *testing.B) {
	const textMessage = 1
	ts := newTestState(b)
	sr := ts.tb.Replacer.Replace
	go clearSamples(ts.tb, ts.samples)

	testCodes := []string{
		sr(`
		var res = ws.connect("WSBIN_URL/ws-compression", {"compression":"deflate"}, (socket) => {
			socket.on('message', (data) => {
				socket.close()
			})
		});
		`),
		sr(`
		var res = ws.connect("WSBIN_URL/ws-compression", {}, (socket) => {
			socket.on('message', (data) => {
				socket.close()
			})
		});
		`),
	}

	ts.tb.Mux.HandleFunc("/ws-compression", http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		kbData := bytes.Repeat([]byte("0123456789"), 100)

		// upgrade connection, send the first (long) message, disconnect
		upgrader := websocket.Upgrader{
			EnableCompression: true,
			ReadBufferSize:    1024,
			WriteBufferSize:   1024,
		}

		conn, e := upgrader.Upgrade(w, req, w.Header())

		if e != nil {
			b.Fatalf("/ws-compression cannot upgrade request: %v", e)
			return
		}

		if e = conn.WriteMessage(textMessage, kbData); e != nil {
			b.Fatalf("/ws-compression cannot write message: %v", e)
			return
		}

		e = conn.Close()
		if e != nil {
			b.Logf("error while closing connection in /ws-compression: %v", e)
			return
		}
	}))

	b.ResetTimer()
	b.Run("compression-enabled", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			if _, err := ts.rt.RunString(testCodes[0]); err != nil {
				b.Error(err)
			}
		}
	})
	b.Run("compression-disabled", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			if _, err := ts.rt.RunString(testCodes[1]); err != nil {
				b.Error(err)
			}
		}
	})
}

func TestCookieJar(t *testing.T) {
	t.Parallel()
	ts := newTestState(t)
	sr := ts.tb.Replacer.Replace

	ts.tb.Mux.HandleFunc("/ws-echo-someheader", http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		responseHeaders := w.Header().Clone()
		if sh, err := req.Cookie("someheader"); err == nil {
			responseHeaders.Add("Echo-Someheader", sh.Value)
		}

		conn, err := (&websocket.Upgrader{}).Upgrade(w, req, responseHeaders)
		if err != nil {
			t.Fatalf("/ws-echo-someheader cannot upgrade request: %v", err)
		}

		err = conn.Close()
		if err != nil {
			t.Logf("error while closing connection in /ws-echo-someheader: %v", err)
		}
	}))
	err := ts.rt.Set("http", common.Bind(ts.rt, httpModule.New().NewModuleInstancePerVU(), ts.ctxPtr))
	require.NoError(t, err)
	ts.state.CookieJar, _ = cookiejar.New(nil)

	_, err = ts.rt.RunString(sr(`
		var res = ws.connect("WSBIN_URL/ws-echo-someheader", function(socket){
			socket.close()
		})
		var someheader = res.headers["Echo-Someheader"];
		if (someheader !== undefined) {
			throw new Error("someheader is echoed back by test server even though it doesn't exist");
		}

		http.cookieJar().set("HTTPBIN_URL/ws-echo-someheader", "someheader", "defaultjar")
		res = ws.connect("WSBIN_URL/ws-echo-someheader", function(socket){
			socket.close()
		})
		someheader = res.headers["Echo-Someheader"];
		if (someheader != "defaultjar") {
			throw new Error("someheader has wrong value "+ someheader + " instead of defaultjar");
		}

		var jar = new http.CookieJar();
		jar.set("HTTPBIN_URL/ws-echo-someheader", "someheader", "customjar")
		res = ws.connect("WSBIN_URL/ws-echo-someheader", {jar: jar}, function(socket){
			socket.close()
		})
		someheader = res.headers["Echo-Someheader"];
		if (someheader != "customjar") {
			throw new Error("someheader has wrong value "+ someheader + " instead of customjar");
		}
		`))
	assert.NoError(t, err)

	assertSessionMetricsEmitted(t, stats.GetBufferedSamples(ts.samples), "", sr("WSBIN_URL/ws-echo-someheader"), statusProtocolSwitch, "")
}
