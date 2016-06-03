package objects

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cenkalti/backoff"
	"github.com/segmentio/go-tableize"
	"github.com/tj/go-sync/semaphore"
)

const (
	// Version of the client library.
	Version = "0.0.1"

	// Endpoint for Segment Objects API.
	DefaultBaseEndpoint = "https://objects.segment.com"
)

type Client struct {
	BaseEndpoint string
	Logger       *log.Logger
	Client       *http.Client

	MaxBatchBytes    int
	MaxBatchCount    int
	MaxBatchInterval time.Duration

	writeKey  string
	wg        sync.WaitGroup
	semaphore semaphore.Semaphore
	closed    int64
	cmap      concurrentMap
}

func New(writeKey string) *Client {
	return &Client{
		BaseEndpoint:     DefaultBaseEndpoint,
		Logger:           log.New(os.Stderr, "segment ", log.LstdFlags),
		writeKey:         writeKey,
		Client:           http.DefaultClient,
		cmap:             NewConcurrentMap(),
		MaxBatchBytes:    500 << 10,
		MaxBatchCount:    100,
		MaxBatchInterval: 10 * time.Second,
		semaphore:        make(semaphore.Semaphore, 10),
	}
}

func (c *Client) fetchFunction(key string) *buffer {
	b := newBuffer(key)
	c.wg.Add(1)
	go c.buffer(b)
	return b
}

func (c *Client) flush(b *buffer) {
	if b.size() == 0 {
		return
	}

	rm := bytes.Join(b.buf, []byte{','})
	rm = append([]byte{'['}, rm...)
	rm = append(rm, ']')
	c.semaphore.Run(func() {
		batchRequest := &batch{
			Collection: b.collection,
			WriteKey:   c.writeKey,
			Objects:    rm,
		}

		c.makeRequest(batchRequest)
	})
	b.reset()
}

func (c *Client) buffer(b *buffer) {
	defer c.wg.Done()

	tick := time.NewTicker(c.MaxBatchInterval)

	for {
		select {
		case req := <-b.Channel:
			req.Properties = tableize.Tableize(req.Properties)
			x, err := json.Marshal(req)
			if err != nil {
				log.Printf("[Error] Message `%s` excluded from batch: %v", req.ID, err)
				continue
			}
			if b.size()+len(x) >= c.MaxBatchBytes || b.count()+1 >= c.MaxBatchCount {
				c.flush(b)
			}
			b.add(x)
		case <-tick.C:
			c.flush(b)
		case <-b.Exit:
			for req := range b.Channel {
				req.Properties = tableize.Tableize(req.Properties)
				x, err := json.Marshal(req)
				if err != nil {
					log.Printf("[Error] Message `%s` excluded from batch: %v", req.ID, err)
					continue
				}
				if b.size()+len(x) >= c.MaxBatchBytes || b.count()+1 >= c.MaxBatchCount {
					c.flush(b)
				}
				b.add(x)
			}
			c.flush(b)
			return
		}
	}

}

func (c *Client) Close() {
	if atomic.LoadInt64(&c.closed) == 1 {
		return
	}
	atomic.AddInt64(&c.closed, 1)

	for t := range c.cmap.Iter() {
		close(t.Val.Channel)
		t.Val.Exit <- struct{}{}
		close(t.Val.Exit)
	}

	c.wg.Wait()
	c.semaphore.Wait()
}

func (c *Client) Set(v *Object) {
	if atomic.LoadInt64(&c.closed) == 1 {
		return
	}
	c.cmap.Fetch(v.Collection, c.fetchFunction).Channel <- v
}

func (c *Client) makeRequest(request *batch) {
	payload, err := json.Marshal(request)
	if err != nil {
		log.Printf("[Error] Batch failed to marshal: %v - %v", request, err)
		return
	}

	bodyReader := bytes.NewReader(payload)

	b := backoff.NewExponentialBackOff()
	b.MaxElapsedTime = 10 * time.Second
	err = backoff.Retry(func() error {
		resp, err := http.Post(c.BaseEndpoint+"/v1/set", "application/json", bodyReader)
		if err != nil {
			return err
		}
		defer resp.Body.Close()

		response := map[string]interface{}{}
		dec := json.NewDecoder(resp.Body)
		dec.Decode(&response)

		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("HTTP Post Request Failed, Status Code %d: %v", resp.StatusCode, response)
		}

		return nil
	}, b)

	if err != nil {
		log.Printf("[Error] %v", err)
		return
	}
}