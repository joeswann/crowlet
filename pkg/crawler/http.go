package crawler

import (
	"io"
	"net/http"
	"net/url"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/tcnksm/go-httpstat"
)

// HTTPResponse holds information from a GET to a specific URL
type HTTPResponse struct {
	URL        string
	Response   *http.Response
	Result     *httpstat.Result
	StatusCode int
	EndTime    time.Time
	Err        error
	Links      []Link
}

// HTTPConfig hold settings used to get pages via HTTP/S
type HTTPConfig struct {
	User       string
	Pass       string
	Timeout    time.Duration
	ParseLinks bool
	Headers    map[string]string // Keep Headers field from your original version
}

type HTTPGetter func(client *http.Client, url string, config HTTPConfig) (response *HTTPResponse)

func createRequest(url string) (*http.Request, *httpstat.Result, error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		// Log error at creation site, but return it for handling
		log.Errorf("Error creating request for %s: %v", url, err)
		return nil, nil, err
	}

	// create a httpstat powered context
	result := &httpstat.Result{}
	ctx := httpstat.WithHTTPStat(req.Context(), result)
	req = req.WithContext(ctx)

	return req, result, nil
}

func configureRequest(req *http.Request, config HTTPConfig) {
	// Set custom headers first, allowing user to override defaults like User-Agent
	for key, value := range config.Headers {
		req.Header.Set(key, value)
		log.Debugf("Setting request header for %s: %s: %s", req.URL.String(), key, value)
	}

	// Set default User-Agent only if not provided by the user
	if _, exists := config.Headers["User-Agent"]; !exists {
		req.Header.Set("User-Agent", "Crowlet/"+VERSION)
		log.Debugf("Setting default User-Agent for %s: %s", req.URL.String(), "Crowlet/"+VERSION)
	}

	if len(config.User) > 0 {
		req.SetBasicAuth(config.User, config.Pass)
		log.Debugf("Setting Basic Auth for %s", req.URL.String())
	}
}

func HTTPGet(client *http.Client, urlStr string, config HTTPConfig) (response *HTTPResponse) {
	response = &HTTPResponse{
		URL: urlStr,
	}

	req, result, err := createRequest(urlStr)
	if err != nil {
		response.Err = err
		// Don't print result here, as the request wasn't even made
		return // Return immediately on request creation error
	}
	response.Result = result // Assign result even if client.Do fails later

	configureRequest(req, config)

	resp, err := client.Do(req)
	response.EndTime = time.Now()
	response.Response = resp

	// Defer closing the response body and printing the result
	defer func() {
		if resp != nil && resp.Body != nil {
			// Ensure body is read and closed IF ParseLinks is false OR if there was an error during parsing
			// If ParseLinks is true and successful, ExtractLinks handles reading/closing
			shouldDiscard := !config.ParseLinks
			if config.ParseLinks && (resp.StatusCode < 200 || resp.StatusCode >= 300 || response.Err != nil) {
				// If we intended to parse links but couldn't (bad status or previous error), still need to discard body
				shouldDiscard = true
			}

			if shouldDiscard {
				_, copyErr := io.Copy(io.Discard, resp.Body) // Read to EOF to allow connection reuse
				if copyErr != nil && response.Err == nil { // Don't overwrite existing error
					log.Warnf("Error discarding response body for %s: %v", urlStr, copyErr)
					// This isn't usually a critical error for the crawl itself
				}
			}
			closeErr := resp.Body.Close()
			if closeErr != nil && response.Err == nil { // Don't overwrite existing error
				log.Warnf("Error closing response body for %s: %v", urlStr, closeErr)
				// Potentially store this minor error if needed, but usually just log
			}
		}
		PrintResult(response) // Print result after body is handled
	}()

	if err != nil {
		log.Errorf("HTTP client error for %s: %v", urlStr, err)
		response.Err = err
		// Attempt to determine status code from error type (e.g., timeout)
		if urlErr, ok := err.(*url.Error); ok {
			if urlErr.Timeout() {
				response.StatusCode = http.StatusGatewayTimeout // 504 seems appropriate for client timeout
			}
			// Could check for other specific net errors here if needed
		}
		// If status code is still 0 after error handling, set a generic client error code?
		if response.StatusCode == 0 {
			 response.StatusCode = 599 // Using a non-standard code to indicate client-side issue
		}
		return // Return after handling client error
	}

	// No client error, proceed with status code and potential link parsing
	response.StatusCode = resp.StatusCode

	if config.ParseLinks && resp.StatusCode >= 200 && resp.StatusCode < 300 {
		currentURL, parseErr := url.Parse(urlStr)
		if parseErr != nil {
			log.Errorf("Error parsing base URL '%s' for link extraction: %v", urlStr, parseErr)
			response.Err = parseErr // Store the parsing error
		} else {
			// ExtractLinks will now read and close the body
			response.Links, err = ExtractLinks(resp.Body, *currentURL)
			if err != nil {
				log.Errorf("Error extracting page links from %s: %v", urlStr, err)
				// Decide if link extraction error should be stored in response.Err
				// response.Err = err // Optional: Overwrite or append? For now, just log.
			}
		}
	}

	return
}

// ConcurrentHTTPGetter interface remains the same
type ConcurrentHTTPGetter interface {
	ConcurrentHTTPGet(urls []string, config HTTPConfig, maxConcurrent int,
		quit <-chan struct{}) <-chan *HTTPResponse
}

// BaseConcurrentHTTPGetter implementation remains the same
type BaseConcurrentHTTPGetter struct {
	Get HTTPGetter
}

func (getter *BaseConcurrentHTTPGetter) ConcurrentHTTPGet(urls []string, config HTTPConfig,
	maxConcurrent int, quit <-chan struct{}) <-chan *HTTPResponse {

	resultChan := make(chan *HTTPResponse, len(urls)) // Buffered channel

	go RunConcurrentGet(getter.Get, urls, config, maxConcurrent, resultChan, quit)

	return resultChan
}

// RunConcurrentGet implementation remains the same
func RunConcurrentGet(httpGet HTTPGetter, urls []string, config HTTPConfig,
	maxConcurrent int, resultChan chan<- *HTTPResponse, quit <-chan struct{}) {

	var wg sync.WaitGroup
	semaphore := make(chan struct{}, maxConcurrent)

	// Create a shared client with potential transport customization
	// Consider customizing transport for things like idle connections, TLS settings etc.
	// transport := &http.Transport{
	// 	// MaxIdleConnsPerHost: maxConcurrent, // Example setting
	// }
	sharedClient := &http.Client{
		Timeout: config.Timeout,
		// Transport: transport, // Uncomment to use custom transport
		// Disable redirects if needed:
		// CheckRedirect: func(req *http.Request, via []*http.Request) error {
		// 	return http.ErrUseLastResponse
		// },
	}

	defer close(resultChan)

	for _, url := range urls {
		select {
		case <-quit:
			log.Info("Stop signal received, waiting for active workers to finish...")
			goto cleanup // Use goto for cleaner exit from loop/select
		case semaphore <- struct{}{}: // Acquire semaphore slot
			wg.Add(1)
			go func(targetURL string) {
				defer func() {
					<-semaphore // Release semaphore slot
					wg.Done()
				}()

				// Check quit signal again before making the request
				select {
				case <-quit:
					log.Debugf("Worker for %s stopping before request due to quit signal", targetURL)
					return // Don't send result if quitting before request
				default:
					// Proceed with request
					resultChan <- httpGet(sharedClient, targetURL, config)
				}

			}(url) // Pass url to goroutine explicitly
		}
	}

cleanup:
	wg.Wait()
	log.Debug("All workers finished.")
}

// PrintResult function remains the same
func PrintResult(result *HTTPResponse) {
	total := int(result.Result.Total(result.EndTime).Round(time.Millisecond) / time.Millisecond)

	// Handle cases where Result might be nil (e.g., request creation failed)
	dnsLookup, tcpConn, tlsHs, serverProc, contentTrans := 0, 0, 0, 0, 0
	if result.Result != nil {
		dnsLookup = int(result.Result.DNSLookup / time.Millisecond)
		tcpConn = int(result.Result.TCPConnection / time.Millisecond)
		tlsHs = int(result.Result.TLSHandshake / time.Millisecond)
		serverProc = int(result.Result.ServerProcessing / time.Millisecond)
		contentTrans = int(result.Result.ContentTransfer(result.EndTime) / time.Millisecond)
	}


	fields := log.Fields{
		"status":     result.StatusCode,
		"total-time": total,
	}

	// Add detailed timing only for debug level
	if log.GetLevel() == log.DebugLevel {
		fields["dns"] = dnsLookup
		fields["tcpconn"] = tcpConn
		fields["tls"] = tlsHs
		fields["server"] = serverProc
		fields["content"] = contentTrans
		// fields["close"] = result.EndTime // Usually not needed for summary
	}

	// Include error in log if present
	if result.Err != nil {
		fields["error"] = result.Err.Error()
		log.WithFields(fields).Error("url=" + result.URL)
	} else if log.GetLevel() <= log.InfoLevel && !log.IsLevelEnabled(log.DebugLevel) {
		// Standard info log without debug details
		log.WithFields(fields).Info("url=" + result.URL)
	} else {
		// Debug log includes detailed timing
		log.WithFields(fields).Debug("url=" + result.URL)
	}
}

// VERSION constant needs to be defined or imported if used here
// It's better practice to pass it via config if needed,
// but for simplicity, we can redefine it here if pkg/crawler is self-contained.
const VERSION = "v0.3.0" // Or import from main/config
