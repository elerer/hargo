package hargo

import (
	"bufio"
	"fmt"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"sync"
	"time"

	log "github.com/Sirupsen/logrus"
	client "github.com/influxdata/influxdb/client/v2"
)

var useInfluxDB = true // just in case we can't connect, run tests without recording results

// LoadTest executes all HTTP requests in order concurrently
// for a given number of workers.
func LoadTest(harfile string, r *bufio.Reader, workers int, timeout time.Duration, u url.URL, ignoreHarCookies bool, isHartest bool) error {

	c, err := NewInfluxDBClient(u)

	if err != nil {
		useInfluxDB = false
		log.Warn("No test results will be recorded to InfluxDB")
	} else {
		log.Info("Recording results to InfluxDB: ", u.String())
	}

	har, err := Decode(r)

	check(err)

	var wg sync.WaitGroup

	log.Infof("Starting load test with %d workers. Duration %v.", workers, timeout)

	for i := 0; i < workers; i++ {
		wg.Add(workers)
		go processEntries(harfile, &har, &wg, i, c, ignoreHarCookies, isHartest)
	}

	if waitTimeout(&wg, timeout) {
		fmt.Printf("\nTimeout of %.1fs elapsed. Terminating load test.\n", timeout.Seconds())
	} else {
		fmt.Println("Wait group finished")
	}

	return nil
}

func processEntries(harfile string, har *Har, wg *sync.WaitGroup, wid int, c client.Client, ignoreHarCookies bool, isHartest bool) {
	defer wg.Done()

	iter := 0

	var resChan chan *MismatchTransaction

	for {

		testResults := make([]TestResult, 0) // batch results

		jar, _ := cookiejar.New(nil)

		httpClient := http.Client{
			Transport: &http.Transport{
				Proxy: http.ProxyFromEnvironment,
				Dial: (&net.Dialer{
					Timeout:   30 * time.Second,
					KeepAlive: 30 * time.Second,
				}).Dial,
				TLSHandshakeTimeout:   10 * time.Second,
				ResponseHeaderTimeout: 10 * time.Second,
				ExpectContinueTimeout: 1 * time.Second,
			},
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
			Jar: jar,
		}

		resChan = make(chan *MismatchTransaction, len(har.Log.Entries))

		for _, entry := range har.Log.Entries {

			msg := fmt.Sprintf("[%d,%d] %s", wid, iter, entry.Request.URL)

			req, err := EntryToRequest(&entry, ignoreHarCookies)

			check(err)

			jar.SetCookies(req.URL, req.Cookies())

			startTime := time.Now()
			resp, err := httpClient.Do(req)
			endTime := time.Now()
			latency := int(endTime.Sub(startTime) / time.Millisecond)
			method := req.Method

			if err != nil {
				log.Error(err)
				tr := TestResult{
					URL:       req.URL.String(),
					Status:    0,
					StartTime: startTime,
					EndTime:   endTime,
					Latency:   latency,
					Method:    method,
					HarFile:   harfile}

				testResults = append(testResults, tr)

				continue
			}

			if resp != nil {
				resp.Body.Close()
			}

			msg += fmt.Sprintf(" %d %dms", resp.StatusCode, latency)

			log.Debug(msg)

			tr := TestResult{
				URL:       req.URL.String(),
				Status:    resp.StatusCode,
				StartTime: startTime,
				EndTime:   endTime,
				Latency:   latency,
				Method:    method,
				HarFile:   harfile}

			if isHartest {
				go CompareHarVsTestResp(entry, resp, resChan)
			}
			testResults = append(testResults, tr)
		}

		if useInfluxDB {
			log.Debug("Writing batch points to InfluxDB...")
			go WritePoints(c, testResults)
		}

		if isHartest {
			//for _, ts := range testResults {
				//fmt.Println(ts)
			//}
			break
		}

		iter++
	}

	if i := len(resChan); i > 0 {
		fmt.Println("HAR test - %d requests mismatch from original", i)
		for elem := range resChan {
			fmt.Println("-----------------------------------------------")
			fmt.Println(elem)

		}
	}

}

func CompareHarVsTestResp(harEntry Entry, testResponse *http.Response, badReqChan chan *MismatchTransaction) {
		if harEntry.Response.Status != testResponse.StatusCode {
		badReqChan <- &MismatchTransaction{
			harStatusint:harEntry.Response.Status,
			testProxyStatusCode:testResponse.StatusCode,
			url:harEntry.Request.URL,
		}
	}

}

// waitTimeout waits for the waitgroup for the specified max timeout.
// Returns true if waiting timed out.
func waitTimeout(wg *sync.WaitGroup, timeout time.Duration) bool {
	c := make(chan struct{})
	go func() {
		defer close(c)
		wg.Wait()
	}()
	select {
	case <-c:
		return false // completed normally
	case <-time.After(timeout):
		return true // timed out
	}
}
