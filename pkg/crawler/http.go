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
	Headers    map[string]string // Added field for custom headers
}

// HTTPGetter performs a single HTTP/S  to the url, and return information
// related to the result as an HTTPResponse
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
	// Set User-Agent unless explicitly overridden by custom headers
	if _, exists := config.Headers["User-Agent"]; !exists {
		req.Header.Set("User-Agent", "Crowlet/"+VERSION) // Use VERSION from main package (consider passing it)
	}

	if len(config.User) > 0 {
		req.SetBasicAuth(config.User, config.Pass)
	}
	// Add custom headers
	for key, value := range config.Headers {
		// Use Set to overwrite potentially default headers like User-Agent if specified
		req.Header.Set(key, value)
	}
}

// HTTPGet issues a GET request to a single URL and returns an HTTPResponse
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
			// Ensure body is read and closed IF ParseLinks is false
			// If ParseLinks is true, ExtractLinks will handle reading/closing
			if !config.ParseLinks {
				_, _ = io.Copy(io.Discard, resp.Body) // Read to EOF to allow connection reuse
			}
			resp.Body.Close()
		}
		// Print result regardless of HTTP errors, but maybe not on request creation errors
		PrintResult(response)
	}()

	// Handle client/transport errors (e.g., connection refused, timeout)
	if err != nil {
		log.Errorf("HTTP client error for %s: %v", urlStr, err)
		response.Err = err
		// Status code is often 0 or irrelevant in these cases
		if urlErr, ok := err.(*url.Error); ok {
			if urlErr.Timeout() {
				response.StatusCode = http.StatusGatewayTimeout // Or a custom code?
			}
		}
		// If status code is 0, maybe set a default like 599? or leave as 0?
		if response.StatusCode == 0 {
			// Let's keep it 0 to indicate transport-level failure clearly
		}
		return // Return after handling client error
	}

	// No client error, proceed to record status code and potentially parse links
	response.StatusCode = resp.StatusCode

	if config.ParseLinks && resp.StatusCode >= 200 && resp.StatusCode < 300 {
		// Only attempt to parse links on successful responses
		currentURL, parseErr := url.Parse(urlStr)
		if parseErr != nil {
			log.Errorf("Error parsing base URL '%s' for link extraction: %v", urlStr, parseErr)
			// Continue without parsing links, response status is still valid
		} else {
			response.Links, err = ExtractLinks(resp.Body, *currentURL) // ExtractLinks now reads/closes body
			if err != nil {
				log.Errorf("Error extracting page links from %s: %v", urlStr, err)
				// Continue, response status is still valid
			}
		}
	}

	return
}

// ConcurrentHTTPGetter allows concurrent execution of an HTTPGetter
type ConcurrentHTTPGetter interface {
	ConcurrentHTTPGet(urls []string, config HTTPConfig, maxConcurrent int,
		quit <-chan struct{}) <-chan *HTTPResponse
}

// BaseConcurrentHTTPGetter implements HTTPGetter interface using net/http package
type BaseConcurrentHTTPGetter struct {
	Get HTTPGetter
	// Consider adding VERSION here if needed for User-Agent
}

// ConcurrentHTTPGet will GET the urls passed and result the results of the crawling
func (getter *BaseConcurrentHTTPGetter) ConcurrentHTTPGet(urls []string, config HTTPConfig,
	maxConcurrent int, quit <-chan struct{}) <-chan *HTTPResponse {

	resultChan := make(chan *HTTPResponse, len(urls)) // Buffered channel

	go RunConcurrentGet(getter.Get, urls, config, maxConcurrent, resultChan, quit)

	return resultChan
}

// RunConcurrentGet runs multiple HTTP requests in parallel, and returns the
// result in resultChan
func RunConcurrentGet(httpGet HTTPGetter, urls []string, config HTTPConfig,
	maxConcurrent int, resultChan chan<- *HTTPResponse, quit <-chan struct{}) {

	var wg sync.WaitGroup
	// Use a semaphore channel to limit concurrency
	semaphore := make(chan struct{}, maxConcurrent)

	// Create a single client with appropriate settings to be shared
	// This encourages connection reuse if keep-alives are enabled server-side
	sharedClient := &http.Client{
		Timeout: config.Timeout,
		// Add transport settings if needed (e.g., TLS config, proxy)
	}

	// Ensure channel is closed when all goroutines complete or function exits
	defer close(resultChan)
	// Ensure semaphore channel is drained and closed? Not strictly necessary.

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
	// Wait for all active goroutines to finish *after* loop exits or quit is received
	wg.Wait()
	log.Debug("All workers finished.")
}

// PrintResult will print information relative to the HTTPResponse
func PrintResult(result *HTTPResponse) {
	// Only print if logging level allows Info or Debug
	if log.GetLevel() < log.WarnLevel {
		total := time.Duration(0)
		// Ensure Result is not nil before accessing fields
		if result.Result != nil {
			total = result.Result.Total(result.EndTime).Round(time.Millisecond)
		}
		totalMs := int(total / time.Millisecond)

		fields := log.Fields{
			"status": result.StatusCode, // Always include status
		}

		// Add timing details only if Result is available and no error occurred
		// or if debug level is enabled
		if result.Result != nil && result.Err == nil {
			fields["total-time"] = totalMs
			if log.GetLevel() == log.DebugLevel {
				fields["dns"] = int(result.Result.DNSLookup / time.Millisecond)
				fields["tcpconn"] = int(result.Result.TCPConnection / time.Millisecond)
				fields["tls"] = int(result.Result.TLSHandshake / time.Millisecond)
				fields["server"] = int(result.Result.ServerProcessing / time.Millisecond)
				fields["content"] = int(result.Result.ContentTransfer(result.EndTime) / time.Millisecond)
				fields["close"] = result.EndTime.Format(time.RFC3339Nano) // More precise time
			}
		} else if result.Err != nil {
			fields["error"] = result.Err.Error() // Include error message if present
		}

		log.WithFields(fields).Info("url=" + result.URL)
	}
}

