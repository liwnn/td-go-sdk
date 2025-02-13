package thinkingdata

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// BatchConsumer upload data to TE by http
type BatchConsumer struct {
	serverUrl   string        // serverUrl
	appId       string        // appId
	timeout     time.Duration // http timeout (mill second)
	compress    bool          // is need compress
	bufferMutex *sync.RWMutex
	cacheMutex  *sync.RWMutex // cache mutex

	buffer        []Data
	batchSize     int      // flush event count each time
	cacheBuffer   [][]Data // buffer
	cacheCapacity int      // buffer max count
}

type BatchConfig struct {
	ServerUrl     string // serverUrl
	AppId         string // appId
	BatchSize     int    // flush event count each time
	Timeout       int    // http timeout (mill second)
	Compress      bool   // enable compress data
	AutoFlush     bool   // enable auto flush
	Interval      int    // auto flush spacing (second)
	CacheCapacity int    // cache event count
}

const (
	DefaultTimeOut       = 30000
	DefaultBatchSize     = 20
	MaxBatchSize         = 200
	DefaultInterval      = 30
	DefaultCacheCapacity = 50
)

// NewBatchConsumer create BatchConsumer
func NewBatchConsumer(serverUrl string, appId string) (Consumer, error) {
	config := BatchConfig{
		ServerUrl: serverUrl,
		AppId:     appId,
		Compress:  true,
	}
	return initBatchConsumer(config)
}

// NewBatchConsumerWithBatchSize create BatchConsumer
// serverUrl
// appId
// batchSize: flush event count each time
func NewBatchConsumerWithBatchSize(serverUrl string, appId string, batchSize int) (Consumer, error) {
	config := BatchConfig{
		ServerUrl: serverUrl,
		AppId:     appId,
		Compress:  true,
		BatchSize: batchSize,
	}
	return initBatchConsumer(config)
}

// NewBatchConsumerWithCompress create BatchConsumer
// serverUrl
// appId
// compress: enable data compress
func NewBatchConsumerWithCompress(serverUrl string, appId string, compress bool) (Consumer, error) {
	config := BatchConfig{
		ServerUrl: serverUrl,
		AppId:     appId,
		Compress:  compress,
	}
	return initBatchConsumer(config)
}

func NewBatchConsumerWithConfig(config BatchConfig) (Consumer, error) {
	return initBatchConsumer(config)
}

func initBatchConsumer(config BatchConfig) (Consumer, error) {
	if config.ServerUrl == "" {
		msg := fmt.Sprint("ServerUrl not be empty")
		Logger(msg)
		return nil, errors.New(msg)
	}
	u, err := url.Parse(config.ServerUrl)
	if err != nil {
		return nil, err
	}
	u.Path = "/sync_server"

	var batchSize int
	if config.BatchSize > MaxBatchSize {
		batchSize = MaxBatchSize
	} else if config.BatchSize <= 0 {
		batchSize = DefaultBatchSize
	} else {
		batchSize = config.BatchSize
	}

	var cacheCapacity int
	if config.CacheCapacity <= 0 {
		cacheCapacity = DefaultCacheCapacity
	} else {
		cacheCapacity = config.CacheCapacity
	}

	var timeout int
	if config.Timeout == 0 {
		timeout = DefaultTimeOut
	} else {
		timeout = config.Timeout
	}

	c := &BatchConsumer{
		serverUrl:     u.String(),
		appId:         config.AppId,
		timeout:       time.Duration(timeout) * time.Millisecond,
		compress:      config.Compress,
		bufferMutex:   new(sync.RWMutex),
		cacheMutex:    new(sync.RWMutex),
		batchSize:     batchSize,
		buffer:        make([]Data, 0, batchSize),
		cacheCapacity: cacheCapacity,
		cacheBuffer:   make([][]Data, 0, cacheCapacity),
	}

	var interval int
	if config.Interval == 0 {
		interval = DefaultInterval
	} else {
		interval = config.Interval
	}
	if config.AutoFlush {
		go func() {
			ticker := time.NewTicker(time.Duration(interval) * time.Second)
			defer ticker.Stop()
			for {
				<-ticker.C
				_ = c.Flush()
			}

		}()
	}
	return c, nil
}

func (c *BatchConsumer) Add(d Data) error {
	c.bufferMutex.Lock()
	c.buffer = append(c.buffer, d)
	c.bufferMutex.Unlock()

	if c.getBufferLength() >= c.batchSize || c.getCacheLength() > 0 {
		err := c.Flush()
		return err
	}

	return nil
}

func (c *BatchConsumer) Flush() error {
	c.cacheMutex.Lock()
	defer c.cacheMutex.Unlock()

	c.bufferMutex.Lock()
	defer c.bufferMutex.Unlock()

	if len(c.buffer) == 0 && len(c.cacheBuffer) == 0 {
		return nil
	}

	defer func() {
		if len(c.cacheBuffer) > c.cacheCapacity {
			c.cacheBuffer = c.cacheBuffer[1:]
		}
	}()

	if len(c.cacheBuffer) == 0 || len(c.buffer) >= c.batchSize {
		c.cacheBuffer = append(c.cacheBuffer, c.buffer)
		c.buffer = make([]Data, 0, c.batchSize)
	}

	err := c.uploadEvents()

	return err
}

func (c *BatchConsumer) uploadEvents() error {
	buffer := c.cacheBuffer[0]

	jsonBytes, err := json.Marshal(buffer)
	if err == nil {
		params := parseTime(jsonBytes)
		for i := 0; i < 3; i++ {
			statusCode, code, err := c.send(params, len(buffer))
			if statusCode == 200 {
				c.cacheBuffer = c.cacheBuffer[1:]
				switch code {
				case 0:
					Logger("send success： %v", params)
					return nil
				case 1, -1:
					msg := "ThinkingDataError:invalid data format"
					Logger(msg)
					return fmt.Errorf(msg)
				case -2:
					msg := "ThinkingDataError:APP ID doesn't exist"
					Logger(msg)
					return fmt.Errorf(msg)
				case -3:
					msg := "ThinkingDataError:invalid ip transmission"
					Logger(msg)
					return fmt.Errorf(msg)
				default:
					msg := "ThinkingDataError:unknown error"
					Logger(msg)
					return fmt.Errorf(msg)
				}
			}
			if err != nil {
				if i == 2 {
					return err
				}
			}
		}
	}
	return nil
}

func (c *BatchConsumer) FlushAll() error {
	for c.getCacheLength() > 0 || c.getBufferLength() > 0 {
		if err := c.Flush(); err != nil {
			if !strings.Contains(err.Error(), "ThinkingDataError") {
				return err
			}
		}
	}
	return nil
}

func (c *BatchConsumer) Close() error {
	return c.FlushAll()
}

func (c *BatchConsumer) IsStringent() bool {
	return false
}

func (c *BatchConsumer) send(data string, size int) (statusCode int, code int, err error) {
	var encodedData string
	var compressType = "gzip"
	if c.compress {
		encodedData, err = encodeData(data)
	} else {
		encodedData = data
		compressType = "none"
	}
	if err != nil {
		return 0, 0, err
	}
	postData := bytes.NewBufferString(encodedData)

	var resp *http.Response
	req, _ := http.NewRequest("POST", c.serverUrl, postData)
	req.Header["appid"] = []string{c.appId}
	req.Header.Set("user-agent", "ta-go-sdk")
	req.Header.Set("version", SdkVersion)
	req.Header.Set("compress", compressType)
	req.Header["TA-Integration-Type"] = []string{LibName}
	req.Header["TA-Integration-Version"] = []string{SdkVersion}
	req.Header["TA-Integration-Count"] = []string{strconv.Itoa(size)}
	client := &http.Client{Timeout: c.timeout}
	resp, err = client.Do(req)

	if err != nil {
		return 0, 0, err
	}

	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		body, _ := ioutil.ReadAll(resp.Body)
		var result struct {
			Code int
		}

		err = json.Unmarshal(body, &result)
		if err != nil {
			return resp.StatusCode, 1, err
		}

		return resp.StatusCode, result.Code, nil
	} else {
		return resp.StatusCode, -1, nil
	}
}

// Gzip
func encodeData(data string) (string, error) {
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)

	_, err := gw.Write([]byte(data))
	if err != nil {
		gw.Close()
		return "", err
	}
	gw.Close()

	return string(buf.Bytes()), nil
}

func (c *BatchConsumer) getBufferLength() int {
	c.bufferMutex.RLock()
	defer c.bufferMutex.RUnlock()
	return len(c.buffer)
}

func (c *BatchConsumer) getCacheLength() int {
	c.cacheMutex.RLock()
	defer c.cacheMutex.RUnlock()
	return len(c.cacheBuffer)
}
