package basic

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"testing"
	"time"

	libp2p "github.com/libp2p/go-libp2p"
	peer "github.com/libp2p/go-libp2p-peer"

	host "github.com/libp2p/go-libp2p-host"

	"github.com/ipfs/ipfs-cluster/api"
	"github.com/ipfs/ipfs-cluster/test"
)

type metricFactory struct {
	l       sync.Mutex
	counter int
}

func newMetricFactory() *metricFactory {
	return &metricFactory{
		counter: 0,
	}
}

func (mf *metricFactory) newMetric(n string, p peer.ID) api.Metric {
	mf.l.Lock()
	defer mf.l.Unlock()
	m := api.Metric{
		Name:  n,
		Peer:  p,
		Value: fmt.Sprintf("%d", mf.counter),
		Valid: true,
	}
	m.SetTTL(5 * time.Second)
	mf.counter++
	return m
}

func (mf *metricFactory) count() int {
	mf.l.Lock()
	defer mf.l.Unlock()
	return mf.counter
}

func testPeerMonitor(t *testing.T) *Monitor {
	return testPeerMonitorWithHost(t, nil)
}

func testPeerMonitorWithHost(t *testing.T, h host.Host) *Monitor {
	mock := test.NewMockRPCClientWithHost(t, h)
	cfg := &Config{}
	cfg.Default()
	cfg.CheckInterval = 2 * time.Second
	mon, err := NewMonitor(cfg)
	if err != nil {
		t.Fatal(err)
	}
	mon.SetClient(mock)
	return mon
}

func TestPeerMonitorShutdown(t *testing.T) {
	pm := testPeerMonitor(t)
	err := pm.Shutdown()
	if err != nil {
		t.Error(err)
	}

	err = pm.Shutdown()
	if err != nil {
		t.Error(err)
	}
}

func TestLogMetricConcurrent(t *testing.T) {
	pm := testPeerMonitor(t)
	defer pm.Shutdown()

	var wg sync.WaitGroup
	wg.Add(3)

	f := func() {
		defer wg.Done()
		for i := 0; i < 25; i++ {
			mt := api.Metric{
				Name:  "test",
				Peer:  test.TestPeerID1,
				Value: fmt.Sprintf("%d", time.Now().UnixNano()),
				Valid: true,
			}
			mt.SetTTL(150 * time.Millisecond)
			pm.LogMetric(mt)
			time.Sleep(75 * time.Millisecond)
		}
	}
	go f()
	go f()
	go f()

	time.Sleep(150 * time.Millisecond)
	last := time.Now().Add(-500 * time.Millisecond)

	for i := 0; i <= 20; i++ {
		lastMtrcs := pm.LastMetrics("test")

		if len(lastMtrcs) != 1 {
			t.Error("no valid metrics", len(lastMtrcs), i)
			time.Sleep(75 * time.Millisecond)
			continue
		}

		n, err := strconv.Atoi(lastMtrcs[0].Value)
		if err != nil {
			t.Fatal(err)
		}
		current := time.Unix(0, int64(n))
		if current.Before(last) {
			t.Errorf("expected newer metric: Current: %s, Last: %s", current, last)
		}
		last = current
		time.Sleep(75 * time.Millisecond)
	}

	wg.Wait()
}

func TestPeerMonitorLogMetric(t *testing.T) {
	pm := testPeerMonitor(t)
	defer pm.Shutdown()
	mf := newMetricFactory()

	// dont fill window
	pm.LogMetric(mf.newMetric("test", test.TestPeerID1))
	pm.LogMetric(mf.newMetric("test", test.TestPeerID2))
	pm.LogMetric(mf.newMetric("test", test.TestPeerID3))

	// fill window
	pm.LogMetric(mf.newMetric("test2", test.TestPeerID3))
	pm.LogMetric(mf.newMetric("test2", test.TestPeerID3))
	pm.LogMetric(mf.newMetric("test2", test.TestPeerID3))
	pm.LogMetric(mf.newMetric("test2", test.TestPeerID3))

	lastMetrics := pm.LastMetrics("testbad")
	if len(lastMetrics) != 0 {
		t.Logf("%+v", lastMetrics)
		t.Error("metrics should be empty")
	}

	lastMetrics = pm.LastMetrics("test")
	if len(lastMetrics) != 3 {
		t.Error("metrics should correspond to 3 hosts")
	}

	for _, v := range lastMetrics {
		switch v.Peer {
		case test.TestPeerID1:
			if v.Value != "0" {
				t.Error("bad metric value")
			}
		case test.TestPeerID2:
			if v.Value != "1" {
				t.Error("bad metric value")
			}
		case test.TestPeerID3:
			if v.Value != "2" {
				t.Error("bad metric value")
			}
		default:
			t.Error("bad peer")
		}
	}

	lastMetrics = pm.LastMetrics("test2")
	if len(lastMetrics) != 1 {
		t.Fatal("should only be one metric")
	}
	if lastMetrics[0].Value != fmt.Sprintf("%d", mf.count()-1) {
		t.Error("metric is not last")
	}
}

func TestPeerMonitorPublishMetric(t *testing.T) {
	h, err := libp2p.New(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	pm := testPeerMonitorWithHost(t, h)
	defer pm.Shutdown()
	defer h.Close()
	mf := newMetricFactory()

	metric := mf.newMetric("test", test.TestPeerID1)
	err = pm.PublishMetric(metric)

	// Note mock rpc returns 3 consensus peers and we cannot
	// push to those so an error is in order and indicates
	// things work as expected.
	if err == nil {
		t.Error("expected an error")
	}
}

func TestPeerMonitorAlerts(t *testing.T) {
	pm := testPeerMonitor(t)
	defer pm.Shutdown()
	mf := newMetricFactory()

	mtr := mf.newMetric("test", test.TestPeerID1)
	mtr.SetTTL(0)
	pm.LogMetric(mtr)
	time.Sleep(time.Second)
	timeout := time.NewTimer(time.Second * 5)

	// it should alert twice at least. Alert re-occurrs.
	for i := 0; i < 2; i++ {
		select {
		case <-timeout.C:
			t.Fatal("should have thrown an alert by now")
		case alrt := <-pm.Alerts():
			if alrt.MetricName != "test" {
				t.Error("Alert should be for test")
			}
			if alrt.Peer != test.TestPeerID1 {
				t.Error("Peer should be TestPeerID1")
			}
		}
	}
}
