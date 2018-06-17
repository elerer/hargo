package hargo

import (
	"fmt"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"time"

	log "github.com/Sirupsen/logrus"
	client "github.com/influxdata/influxdb/client/v2"
)

var useInfluxDB = true // just in case we can't connect, run tests without recording results

// LoadTest executes all HTTP requests in order concurrently
// for a given number of workers.
func LoadTest(harfile string, har Har, u url.URL, ignoreHarCookies bool, isHartest bool, ch chan int) error {

	c, err := NewInfluxDBClient(u)

	if err != nil {
		useInfluxDB = false
		log.Warn("No test results will be recorded to InfluxDB")
	} else {
		log.Info("Recording results to InfluxDB: ", u.String())
	}

	check(err)

	go processEntries(harfile, har, c, ignoreHarCookies, isHartest, ch)

	return nil
}

func processEntries(harfile string, har Har, c client.Client, ignoreHarCookies bool, isHartest bool, ch chan int) {

	defer func(ch chan int) { <-ch }(ch)

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

		var chanLen int

		chanLen += len(har.Log.Entries)

		resChan = make(chan *MismatchTransaction, chanLen)

		for _, entry := range har.Log.Entries {

			msg := fmt.Sprintf("[%d] %s", iter, entry.Request.URL)

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
			harStatusint:        harEntry.Response.Status,
			testProxyStatusCode: testResponse.StatusCode,
			url:                 harEntry.Request.URL,
		}
	}

}
