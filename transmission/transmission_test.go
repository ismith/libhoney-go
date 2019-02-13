package transmission

import (
	"bytes"
	"compress/gzip"
	"crypto/rand"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

var (
	placeholder = Response{StatusCode: http.StatusTeapot}
)

type errReader struct{}

func (e errReader) Read(b []byte) (int, error) { return 0, errors.New("mystery read error!") }

func TestTxAdd(t *testing.T) {
	txDC := &txDefaultClient{logger: &nullLogger{}}
	txDC.muster.Work = make(chan interface{}, 1)
	dc.responses = make(chan Response, 1)
	dc.responses <- placeholder
	txDC.responses = dc.responses

	// default successful case
	e := &Event{Metadata: "mmeetta", client: dc}
	txDC.Add(e)
	added := <-txDC.muster.Work
	testEquals(t, e, added)
	rsp := testGetResponse(t, dc.responses)
	testIsPlaceholderResponse(t, rsp, "work was simply queued; no response available yet")

	// make the queue 0 length to force an overflow
	txDC.muster.Work = make(chan interface{}, 0)
	txDC.Add(e)
	rsp = testGetResponse(t, dc.responses)
	testErr(t, rsp.Err)
	testEquals(t, rsp.Err.Error(), "queue overflow",
		"overflow error should have been put on responses channel immediately")
	// make sure that (default) nonblocking on responses allows execution even if
	// responses channel is full
	dc.responses <- placeholder
	txDC.Add(e)
	rsp = testGetResponse(t, dc.responses)
	testIsPlaceholderResponse(t, rsp,
		"placeholder was blocking responses channel but .Add should have continued")

	// test blocking on send still gets it down the channel
	txDC.blockOnSend = true
	txDC.muster.Work = make(chan interface{}, 1)
	dc.responses <- placeholder

	txDC.Add(e)
	added = <-txDC.muster.Work
	testEquals(t, e, added)
	rsp = testGetResponse(t, dc.responses)
	testIsPlaceholderResponse(t, rsp, "blockOnSend doesn't affect the responses queue")

	// test blocking on response still gets an overflow down the channel
	txDC.blockOnSend = false
	txDC.blockOnResponses = true
	txDC.muster.Work = make(chan interface{}, 0)

	dc.responses <- placeholder
	go txDC.Add(e)
	rsp = testGetResponse(t, dc.responses)
	testIsPlaceholderResponse(t, rsp, "should pull placeholder response off channel first")
	rsp = testGetResponse(t, dc.responses)
	testErr(t, rsp.Err)
	testEquals(t, rsp.Err.Error(), "queue overflow",
		"overflow error should have been pushed into channel")
}

type FakeRoundTripper struct {
	req     *http.Request
	reqBody string
	resp    *http.Response
	respErr error
}

func (f *FakeRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	f.req = r
	bodyBytes, _ := ioutil.ReadAll(r.Body)
	f.reqBody = string(bodyBytes)
	return f.resp, f.respErr
}

type testNotifier struct{}

func (tn *testNotifier) Done() {}

// test the mechanics of sending / receiving responses
func TestTxSendSingle(t *testing.T) {
	frt := &FakeRoundTripper{}
	b := &batchAgg{
		httpClient:  &http.Client{Transport: frt},
		testNower:   &fakeNower{},
		testBlocker: &sync.WaitGroup{},
		responses:   make(chan Response, 1),
	}
	reset := func(b *batchAgg, frt *FakeRoundTripper, statusCode int, body string, err error) {
		if body == "" {
			frt.resp = nil
		} else {
			frt.resp = &http.Response{
				StatusCode: statusCode,
				Body:       ioutil.NopCloser(strings.NewReader(body)),
			}
		}
		frt.respErr = err
		b.batches = nil
	}

	fhData := map[string]interface{}{"foo": "bar"}
	e := &Event{
		fieldHolder: fieldHolder{data: fhData},
		SampleRate:  4,
		APIHost:     "http://fakeHost:8080",
		WriteKey:    "written",
		Dataset:     "ds1",
		Metadata:    "emmetta",
	}
	reset(b, frt, 200, `[{"status":202}]`, nil)
	b.Add(e)
	b.Fire(&testNotifier{})
	expectedURL := fmt.Sprintf("%s/1/batch/%s", e.APIHost, e.Dataset)
	testEquals(t, frt.req.URL.String(), expectedURL)
	versionedUserAgent := fmt.Sprintf("libhoney-go/%s", version)
	testEquals(t, frt.req.Header.Get("User-Agent"), versionedUserAgent)
	testEquals(t, frt.req.Header.Get("X-Honeycomb-Team"), e.WriteKey)
	buf := &bytes.Buffer{}
	g := gzip.NewWriter(buf)
	_, err := g.Write([]byte(`[{"data":{"foo":"bar"},"samplerate":4}]`))
	testOK(t, err)
	testOK(t, g.Close())
	testEquals(t, frt.reqBody, buf.String())

	rsp := testGetResponse(t, b.responses)
	testEquals(t, rsp.Duration, time.Second*10)
	testEquals(t, rsp.Metadata, "emmetta")
	testEquals(t, rsp.StatusCode, 202)
	testOK(t, rsp.Err)

	// test UserAgentAddition
	UserAgentAddition = "  fancyApp/3 "
	expectedUserAgentAddition := "fancyApp/3"
	longUserAgent := fmt.Sprintf("%s %s", versionedUserAgent, expectedUserAgentAddition)
	reset(b, frt, 200, `[{"status":202}]`, nil)
	b.Add(e)
	b.Fire(&testNotifier{})
	testEquals(t, frt.req.Header.Get("User-Agent"), longUserAgent)
	rsp = testGetResponse(t, b.responses)
	testEquals(t, rsp.StatusCode, 202)
	testOK(t, rsp.Err)
	UserAgentAddition = ""

	// test unsuccessful send
	reset(b, frt, 0, "", errors.New("testing error handling"))
	b.Add(e)
	b.Fire(&testNotifier{})
	rsp = testGetResponse(t, b.responses)
	testErr(t, rsp.Err)
	testEquals(t, rsp.StatusCode, 0)
	testEquals(t, len(rsp.Body), 0)

	// test nonblocking response path is actually nonblocking, drops response
	b.responses <- placeholder
	reset(b, frt, 0, "", errors.New("err"))
	b.testBlocker.Add(1)
	b.Add(e)
	go b.Fire(&testNotifier{})
	b.testBlocker.Wait() // triggered on drop
	rsp = testGetResponse(t, b.responses)
	testIsPlaceholderResponse(t, rsp,
		"should pull placeholder response and only placeholder response off channel")

	// test blocking response path, error
	b.blockOnResponses = true
	reset(b, frt, 0, "", errors.New("err"))
	b.responses <- placeholder
	b.Add(e)
	go b.Fire(&testNotifier{})
	rsp = testGetResponse(t, b.responses)
	testIsPlaceholderResponse(t, rsp,
		"should pull placeholder response off channel first")
	rsp = testGetResponse(t, b.responses)
	testErr(t, rsp.Err)
	testEquals(t, rsp.StatusCode, 0)
	testEquals(t, len(rsp.Body), 0)

	// test blocking response path, request completed but got HTTP error code
	b.blockOnResponses = true
	reset(b, frt, 400, `{"error":"unknown Team key - check your credentials"}`, nil)
	b.responses <- placeholder
	b.Add(e)
	go b.Fire(&testNotifier{})
	rsp = testGetResponse(t, b.responses)
	testIsPlaceholderResponse(t, rsp,
		"should pull placeholder response off channel first")
	rsp = testGetResponse(t, b.responses)
	testEquals(t, rsp.StatusCode, 400)
	testEquals(t, string(rsp.Body), `{"error":"unknown Team key - check your credentials"}`)

	// test the case that our POST request completed, we got an HTTP error
	// code, but then got an error reading HTTP response body. An unlikely
	// scenario but technically possible.
	b.blockOnResponses = true
	frt.resp = &http.Response{
		StatusCode: 500,
		Body:       ioutil.NopCloser(errReader{}),
	}
	frt.respErr = nil
	b.batches = nil
	b.responses <- placeholder
	b.Add(e)
	go b.Fire(&testNotifier{})
	rsp = testGetResponse(t, b.responses)
	testIsPlaceholderResponse(t, rsp,
		"should pull placeholder response off channel first")
	rsp = testGetResponse(t, b.responses)
	testEquals(t, rsp.Err, errors.New("Got HTTP error code but couldn't read response body: mystery read error!"))

	// test blocking response path, no error
	b.responses <- placeholder
	reset(b, frt, 200, `[{"status":202}]`, nil)
	b.Add(e)
	go b.Fire(&testNotifier{})
	rsp = testGetResponse(t, b.responses)
	testIsPlaceholderResponse(t, rsp,
		"should pull placeholder response off channel first")
	rsp = testGetResponse(t, b.responses)
	testEquals(t, rsp.Duration, time.Second*10)
	testEquals(t, rsp.Metadata, "emmetta")
	testEquals(t, rsp.StatusCode, 202)
	testOK(t, rsp.Err)
}

// test the details of handling batch behavior on a batch with a single dataset
func TestTxSendBatchSingleDataset(t *testing.T) {
	tsts := []struct {
		in       []map[string]interface{} // datas
		response string                   // JSON from server
		expected []Response
	}{
		{
			[]map[string]interface{}{
				{"a": 1},
				{"b": 2, "bb": 22},
				{"c": 3.1},
			},
			`[{"status":202},{"status":202},{"status":429,"error":"bratelimited"}]`,
			[]Response{
				{StatusCode: 202, Metadata: "emmetta0"},
				{StatusCode: 202, Metadata: "emmetta1"},
				{Err: errors.New("bratelimited"), StatusCode: 429, Metadata: "emmetta2"},
			},
		},
		{
			[]map[string]interface{}{
				{"a": 1},
				{"b": func() {}},
				{"c": 3.1},
			},
			`[{"status":202},{"status":202},{"status":202}]`,
			[]Response{
				{StatusCode: 202, Metadata: "emmetta0"},
				{StatusCode: 202, Metadata: "emmetta1"},
				{StatusCode: 202, Metadata: "emmetta2"},
			},
		},
	}

	frt := &FakeRoundTripper{
		resp: &http.Response{
			StatusCode: 200,
		},
	}

	for _, tt := range tsts {
		b := &batchAgg{
			httpClient: &http.Client{Transport: frt},
			responses:  make(chan Response, len(tt.expected)),
		}
		frt.resp.Body = ioutil.NopCloser(strings.NewReader(tt.response))
		for i, data := range tt.in {
			b.Add(&Event{
				fieldHolder: fieldHolder{data: data},
				APIHost:     "fakeHost",
				WriteKey:    "written",
				Dataset:     "ds1",
				Metadata:    fmt.Sprint("emmetta", i), // tracking insertion order
			})
		}
		b.Fire(&testNotifier{})
		for _, expResp := range tt.expected {
			resp := testGetResponse(t, b.responses)
			testEquals(t, resp.StatusCode, expResp.StatusCode)
			testEquals(t, resp.Metadata, expResp.Metadata)
			if expResp.Err != nil {
				if !strings.Contains(resp.Err.Error(), expResp.Err.Error()) {
					t.Errorf("expected error to contain '%s', got: '%s'", expResp.Err.Error(), resp.Err.Error())
				}
			} else {
				testEquals(t, resp.Err, expResp.Err)
			}
		}
	}
}

// FancyFakeRoundTripper gets built with a map of incoming URL/Header components
// to the body that's expected and the response that's appropriate for that
// request. It'll send a different response depending on what it gets as well as
// error if the body was wrong
type FancyFakeRoundTripper struct {
	req       *http.Request
	reqBody   string
	reqBodies map[string]string
	resp      *http.Response

	// map of request apihost/writekey/dataset to intended response
	respBody   string
	respBodies map[string]string
	respErr    error
}

func (f *FancyFakeRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	var bodyFound, responseFound bool
	f.req = r
	for reqHeader, reqBody := range f.reqBodies {
		// respHeader is apihost,writekey,dataset
		headerKeys := strings.Split(reqHeader, ",")
		expectedURL, _ := url.Parse(fmt.Sprintf("%s/1/batch/%s", headerKeys[0], headerKeys[2]))
		if r.Header.Get("X-Honeycomb-Team") == headerKeys[1] && r.URL.String() == expectedURL.String() {
			bodyBytes, _ := ioutil.ReadAll(r.Body)
			f.reqBody = string(bodyBytes)
			// build compressed copy to compare
			expectedBuf := &bytes.Buffer{}
			g := gzip.NewWriter(expectedBuf)
			g.Write([]byte(reqBody))
			g.Close()
			// did we get the body we were expecting?
			if f.reqBody != expectedBuf.String() {
				continue
			}
			f.resp.Body = ioutil.NopCloser(strings.NewReader(reqBody))
			bodyFound = true
			break
		}
	}
	if !bodyFound {
		f.resp.Body = ioutil.NopCloser(strings.NewReader(`{"error":"ffrt body not found"}`))
		return f.resp, f.respErr
	}
	for respHeader, respBody := range f.respBodies {
		// respHeader is apihost,writekey,dataset
		headerKeys := strings.Split(respHeader, ",")
		expectedURL, _ := url.Parse(fmt.Sprintf("%s/1/batch/%s", headerKeys[0], headerKeys[2]))
		if r.Header.Get("X-Honeycomb-Team") == headerKeys[1] && r.URL.String() == expectedURL.String() {
			f.resp.Body = ioutil.NopCloser(strings.NewReader(respBody))
			responseFound = true
			break
		}
	}
	if !responseFound {
		f.resp.Body = ioutil.NopCloser(strings.NewReader(`{"error":"ffrt response not found"}`))
	}
	return f.resp, f.respErr
}

// batch behavior on a batch with a multiple datasets/writekeys/apihosts
func TestTxSendBatchMultiple(t *testing.T) {
	tsts := []struct {
		in           map[string][]map[string]interface{} // datas
		expReqBodies map[string]string
		respBodies   map[string]string // JSON from server
		expected     map[string]Response
	}{
		{
			map[string][]map[string]interface{}{
				"ah1,wk1,ds1": {
					{"a": 1},
					{"b": 2, "bb": 22},
					{"c": 3.1},
				},
				"ah1,wk1,ds2": {
					{"a": 12},
					{"b": 22, "bb": 33},
					{"c": 39.2},
				},
				"ah3,wk3,ds3": {
					{"a": 32},
					{"b": 32, "bb": 39},
					{"c": 3.8},
				},
			},
			map[string]string{
				"ah1,wk1,ds1": `[{"data":{"a":1}},{"data":{"b":2,"bb":22}},{"data":{"c":3.1}}]`,
				"ah1,wk1,ds2": `[{"data":{"a":12}},{"data":{"b":22,"bb":33}},{"data":{"c":39.2}}]`,
				"ah3,wk3,ds3": `[{"data":{"a":32}},{"data":{"b":32,"bb":39}},{"data":{"c":3.8}}]`,
			},
			map[string]string{
				"ah1,wk1,ds1": `[{"status":202},{"status":203},{"status":204}]`,
				"ah1,wk1,ds2": `[{"status":202},{"status":202},{"status":429,"error":"bratelimited"}]`,
				"ah3,wk3,ds3": `[{"status":200},{"status":201},{"status":202}]`,
			},
			map[string]Response{
				"emmetta0": {StatusCode: 202, Metadata: "emmetta0"},
				"emmetta1": {StatusCode: 203, Metadata: "emmetta1"},
				"emmetta2": {StatusCode: 204, Metadata: "emmetta2"},
				"emmetta3": {StatusCode: 202, Metadata: "emmetta3"},
				"emmetta4": {StatusCode: 202, Metadata: "emmetta4"},
				"emmetta5": {Err: errors.New("bratelimited"), StatusCode: 429, Metadata: "emmetta5"},
				"emmetta6": {StatusCode: 200, Metadata: "emmetta6"},
				"emmetta7": {StatusCode: 201, Metadata: "emmetta7"},
				"emmetta8": {StatusCode: 202, Metadata: "emmetta8"},
			},
		},
	}

	ffrt := &FancyFakeRoundTripper{
		resp: &http.Response{
			StatusCode: 200,
		},
	}

	for _, tt := range tsts {
		b := &batchAgg{
			httpClient: &http.Client{Transport: ffrt},
			responses:  make(chan Response, len(tt.expected)),
		}
		ffrt.reqBodies = tt.expReqBodies
		ffrt.respBodies = tt.respBodies
		// insert events in sorted order to check responses
		keys := []string{}
		for k := range tt.in {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var i int // counter to identify response metadata
		for _, k := range keys {
			headers := strings.Split(k, ",")
			for _, data := range tt.in[k] {
				b.Add(&Event{
					fieldHolder: fieldHolder{data: data},
					APIHost:     headers[0],
					WriteKey:    headers[1],
					Dataset:     headers[2],
					Metadata:    fmt.Sprint("emmetta", i), // tracking insertion order
				})
				i++
			}
		}
		b.Fire(&testNotifier{})
		// go through the right number of expected responses, look for matches
		for range tt.expected {
			var found bool
			resp := testGetResponse(t, b.responses)
			for meta, expResp := range tt.expected {
				if meta == resp.Metadata {
					found = true
					testEquals(t, resp.StatusCode, expResp.StatusCode)
					if expResp.Err != nil {
						if !strings.Contains(resp.Err.Error(), expResp.Err.Error()) {
							t.Errorf("expected error to contain '%s', got: '%s'", expResp.Err.Error(), resp.Err.Error())
						}
					} else {
						testEquals(t, resp.Err, expResp.Err)
					}
				}
			}
			if !found {
				t.Errorf("couldn't find expected response for %+v\n", resp)
			}
		}
	}
}

func TestRenqueueEventsAfterOverflow(t *testing.T) {
	frt := &FakeRoundTripper{}
	b := &batchAgg{
		httpClient: &http.Client{Transport: frt},
		testNower:  &fakeNower{},
		responses:  make(chan Response, 1),
	}

	events := make([]*Event, 100)
	// we make the event bodies 99KB to allow for the column name and sampleRate/Timestamp
	// payload
	fhData := map[string]interface{}{"reallyBigColumn": randomString(99 * 1000)}
	for i := range events {
		events[i] = &Event{
			fieldHolder: fieldHolder{data: fhData},
			SampleRate:  4,
			APIHost:     "http://fakeHost:8080",
			WriteKey:    "written",
			Dataset:     "ds1",
			Metadata:    "emmetta",
		}
	}

	reset := func(b *batchAgg, frt *FakeRoundTripper, statusCode int, body string, err error) {
		if body == "" {
			frt.resp = nil
		} else {
			frt.resp = &http.Response{
				StatusCode: statusCode,
				Body:       ioutil.NopCloser(strings.NewReader(body)),
			}
		}
		frt.respErr = err
		b.batches = nil
	}

	key := "http://fakeHost:8080_written_ds1"

	reset(b, frt, 200, `[{"status":202}]`, nil)
	b.fireBatch(events)
	testEquals(t, len(b.overflowBatches), 1)
	testEquals(t, len(b.overflowBatches[key]), 50)
}

type testRoundTripper struct {
	callCount int
}

func (t *testRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	t.callCount++

	ioutil.ReadAll(r.Body)

	return &http.Response{
		StatusCode: 200,
		Body:       ioutil.NopCloser(strings.NewReader(`[{"status":202}]`)),
	}, nil
}

// Verify that events over the batch size limit are requeued and sent
func TestFireBatchLargeEventsSent(t *testing.T) {
	trt := &testRoundTripper{}
	b := &batchAgg{
		httpClient: &http.Client{Transport: trt},
		testNower:  &fakeNower{},
		responses:  make(chan Response, 1),
	}

	events := make([]*Event, 150)
	fhData := map[string]interface{}{"reallyBigColumn": randomString(99 * 1000)}
	for i := range events {
		events[i] = &Event{
			fieldHolder: fieldHolder{data: fhData},
			SampleRate:  4,
			APIHost:     "http://fakeHost:8080",
			WriteKey:    "written",
			Dataset:     "ds1",
			Metadata:    "emmetta",
		}
		b.Add(events[i])
	}

	key := "http://fakeHost:8080_written_ds1"

	b.Fire(&testNotifier{})
	testEquals(t, len(b.overflowBatches), 0)
	testEquals(t, len(b.overflowBatches[key]), 0)
	testEquals(t, trt.callCount, 3)
}

// Ensure we handle events greater than the limit by enqueuing a response
func TestFireBatchWithTooLargeEvent(t *testing.T) {
	trt := &testRoundTripper{}
	b := &batchAgg{
		httpClient:  &http.Client{Transport: trt},
		testNower:   &fakeNower{},
		testBlocker: &sync.WaitGroup{},
		responses:   make(chan Response, 1),
	}

	events := make([]*Event, 1)
	for i := range events {
		fhData := map[string]interface{}{"reallyREALLYBigColumn": randomString(1024 * 1024)}
		events[i] = &Event{
			fieldHolder: fieldHolder{data: fhData},
			SampleRate:  4,
			APIHost:     "http://fakeHost:8080",
			WriteKey:    "written",
			Dataset:     "ds1",
			Metadata:    fmt.Sprintf("meta %d", i),
		}
		b.Add(events[i])
	}

	key := "http://fakeHost:8080_written_ds1"

	b.Fire(&testNotifier{})
	b.testBlocker.Wait()
	resp := testGetResponse(t, b.responses)
	testEquals(t, resp.Err.Error(), "event exceeds max event size of 100000 bytes, API will not accept this event")

	testEquals(t, len(b.overflowBatches), 0)
	testEquals(t, len(b.overflowBatches[key]), 0)
	testEquals(t, trt.callCount, 0)

}

func TestWriterOutput(t *testing.T) {
	buf := bytes.NewBuffer(nil)
	writer := WriterOutput{
		W: buf,
	}
	ev := Event{
		Timestamp:  time.Time{},
		SampleRate: 1,
		fieldHolder: fieldHolder{
			data: marshallableMap{},
		},
	}

	writer.Add(&ev)
	testEquals(t, strings.TrimSpace(buf.String()), `{"data":{}}`)

	ev.Timestamp = ev.Timestamp.Add(time.Second)
	ev.SampleRate = 2
	ev.Dataset = "dataset"
	ev.AddField("key", "val")

	buf.Reset()
	writer.Add(&ev)
	testEquals(
		t,
		strings.TrimSpace(buf.String()),
		`{"data":{"key":"val"},"samplerate":2,"time":"0001-01-01T00:00:01Z","dataset":"dataset"}`,
	)
}

func randomString(length int) string {
	b := make([]byte, length/2)
	rand.Read(b)
	return fmt.Sprintf("%x", b)
}