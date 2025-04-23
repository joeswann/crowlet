package main

import (
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
	"net/url"

	exec "github.com/Pixep/crowlet/internal/pkg"
	"github.com/Pixep/crowlet/pkg/crawler"

	log "github.com/sirupsen/logrus"
	"github.com/urfave/cli"
)

// VERSION stores the current version as string
var VERSION = "v0.3.0"

func beforeApp(c *cli.Context) error {
	if c.GlobalBool("debug") {
		log.SetLevel(log.DebugLevel)
	} else if c.GlobalBool("quiet") || c.GlobalBool("summary-only") {
		log.SetLevel(log.FatalLevel)
	}

	if c.GlobalBool("json") {
		log.SetFormatter(&log.JSONFormatter{})
	}

	if c.NArg() < 1 {
		log.Error("sitemap url required")
		cli.ShowAppHelpAndExit(c, 2)
	}

	// Validate header format early
	headerStrings := c.GlobalStringSlice("header")
	for _, h := range headerStrings {
		parts := strings.SplitN(h, ":", 2)
		if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
			log.Errorf("Invalid header format: '%s'. Use 'Key: Value'", h)
			cli.ShowAppHelpAndExit(c, 3)
		}
	}

	if len(c.GlobalString("pre-cmd")) > 1 {
		ok := exec.Exec(c.GlobalString("pre-cmd"))
		if !ok {
			log.Fatal("Failed to execute pre-execution command")
		}
	}

	return nil
}

func afterApp(c *cli.Context) error {
	if len(c.GlobalString("post-cmd")) > 1 {
		ok := exec.Exec(c.GlobalString("post-cmd"))
		if !ok {
			log.Fatal("Failed to execute post-execution command")
		}
	}

	return nil
}

var exitCode int

func main() {
	app := cli.NewApp()
	app.Name = "crowlet"
	app.Version = VERSION
	app.Usage = "a basic sitemap.xml crawler"
	app.Action = start
	app.UsageText = "[global options] sitemap-url"
	app.Before = beforeApp
	app.After = afterApp
	app.Flags = []cli.Flag{
		cli.BoolFlag{
			Name:  "crawl-hyperlinks",
			Usage: "follow and test hyperlinks ('a' tags href)",
		},
		cli.BoolFlag{
			Name:  "crawl-images",
			Usage: "follow and test image links ('img' tags src)",
		},
		cli.BoolFlag{
			Name:  "crawl-external",
			Usage: "follow and test external links. Use in combination with 'follow-hyperlinks' and/or 'follow-images'",
		},
		cli.BoolFlag{
			Name:  "forever,f",
			Usage: "crawl the sitemap's URLs forever... or until stopped",
		},
		cli.IntFlag{
			Name:  "iterations,i",
			Usage: "number of crawling iterations for the whole sitemap",
			Value: 1,
		},
		cli.IntFlag{
			Name:   "wait-interval,w",
			Usage:  "wait interval in seconds between sitemap crawling iterations",
			EnvVar: "CRAWL_WAIT_INTERVAL",
			Value:  0,
		},
		cli.IntFlag{
			Name:   "throttle,t",
			Usage:  "number of http requests to do at once",
			EnvVar: "CRAWL_THROTTLE",
			Value:  5,
		},
		cli.IntFlag{
			Name:  "timeout,y",
			Usage: "timeout duration for requests, in milliseconds",
			Value: 20000,
		},
		cli.BoolFlag{
			Name:  "quiet,silent,q",
			Usage: "suppress all normal output",
		},
		cli.BoolFlag{
			Name:  "json,j",
			Usage: "output using JSON format (experimental)",
		},
		cli.IntFlag{
			Name: "non-200-error,e",
			Usage: "error code to use if any non-200 response is encountered",
			Value: 1,
		},
		cli.IntFlag{
			Name: "response-time-error,l",
			Usage: "error code to use if the maximum response time is overrun",
			Value: 1,
		},
		cli.IntFlag{
			Name: "response-time-max,m",
			Usage: "maximum response time of URLs, in milliseconds, before considered an error",
			Value: 0,
		},
		cli.BoolFlag{
			Name:  "summary-only",
			Usage: "print only the summary",
		},
		cli.StringFlag{
			Name:   "override-host",
			Usage:  "override the hostname used in sitemap urls",
			EnvVar: "CRAWL_HOST",
		},
		// New Scheme Override Flag
		cli.StringFlag{
			Name:  "override-scheme",
			Usage: "override the URL scheme (protocol) used in sitemap URLs (e.g. http or https)",
		},
		// Header Flag
		cli.StringSliceFlag{
			Name:  "header, H",
			Usage: "Add custom header(s) to the request (e.g., --header \"X-Api-Key: value\")",
		},
		cli.StringFlag{
			Name:  "pre-cmd",
			Usage: "command(s) to run before starting crawler",
		},
		cli.StringFlag{
			Name:  "post-cmd",
			Usage: "command(s) to run after crawler finishes",
		},
		cli.BoolFlag{
			Name:  "debug",
			Usage: "run in debug mode",
		},
	}

	err := app.Run(os.Args)
	if err != nil {
		log.Error(err)
		if exitCode == 0 {
			exitCode = 1
		}
	}
	os.Exit(exitCode)
}

func addInterruptHandlers() chan struct{} {
	stop := make(chan struct{})
	osSignal := make(chan os.Signal, 1)
	signal.Notify(osSignal, os.Interrupt, syscall.SIGTERM)
	signal.Notify(osSignal, os.Interrupt, syscall.SIGINT)

	go func() {
		<-osSignal
		log.Warn("Interrupt signal received")
		close(stop)
	}()

	return stop
}

func runMainLoop(urls []string, config crawler.CrawlConfig, iterations int, forever bool, waitInterval int) (stats crawler.CrawlStats) {
	for i := 0; i < iterations || forever; i++ {
		if i != 0 {
			time.Sleep(time.Duration(waitInterval) * time.Second)
		}

		quit := addInterruptHandlers()
		itStats, err := crawler.AsyncCrawl(urls, config, quit)

		stats = crawler.MergeCrawlStats(stats, itStats)

		if err != nil {
			log.Warn(err)
		}

		select {
		case <-quit:
			return
		default:
		}
	}

	return
}

func start(c *cli.Context) error {
	sitemapURL := c.Args().Get(0)
	log.Info("Crawling ", sitemapURL)

	urls, err := crawler.GetSitemapUrlsAsStrings(sitemapURL)
	if err != nil {
		log.Fatal(err)
	}
	log.Info("Found ", len(urls), " URL(s)")

	// Process scheme override
	scheme := c.String("override-scheme")
	if scheme != "" {
		for i, u := range urls {
			parsed, err := url.Parse(u)
			if err != nil {
				log.Warnf("Skipping scheme override for invalid URL %s: %v", u, err)
				continue
			}
			parsed.Scheme = scheme
			urls[i] = parsed.String()
			log.Debugf("Overriding scheme: %s", urls[i])
		}
	}

	// Process headers
	headerStrings := c.StringSlice("header")
	headersMap := make(map[string]string)
	for _, h := range headerStrings {
		parts := strings.SplitN(h, ":", 2)
		if len(parts) == 2 {
			key := strings.TrimSpace(parts[0])
			value := strings.TrimSpace(parts[1])
			if key != "" {
	headersMap[key] = value
				log.Debugf("Adding header: '%s: %s'", key, value)
			} else {
				log.Warnf("Skipping header with empty key: '%s'", h)
			}
		}
	}

	config := crawler.CrawlConfig{
		Throttle: c.Int("throttle"),
		Host:     c.String("override-host"),
		HTTP: crawler.HTTPConfig{
			User:    c.String("user"),
			Pass:    c.String("pass"),
			Timeout: time.Duration(c.Int("timeout")) * time.Millisecond,
			Headers: headersMap,
		},
		HTTPGetter: &crawler.BaseConcurrentHTTPGetter{
			Get: crawler.HTTPGet,
		},
		Links: crawler.CrawlPageLinksConfig{
			CrawlExternalLinks: c.Bool("crawl-external"),
			CrawlImages:        c.Bool("crawl-images"),
			CrawlHyperlinks:    c.Bool("crawl-hyperlinks"),
		},
	}
	config.HTTP.ParseLinks = config.Links.CrawlExternalLinks || config.Links.CrawlHyperlinks || config.Links.CrawlImages

	stats := runMainLoop(urls, config, c.Int("iterations"), c.Bool("forever"), c.Int("wait-interval"))
	if !c.GlobalBool("quiet") {
		if c.GlobalBool("json") {
			crawler.PrintJSONSummary(stats)
		} else {
			crawler.PrintSummary(stats)
		}
	}

	if stats.Total > 0 && stats.StatusCodes[200] != stats.Total {
		log.Warnf("%d out of %d crawled URLs had non-200 status codes.", stats.Total-stats.StatusCodes[200], stats.Total)
		exitCode = c.Int("non-200-error")
		return nil
	}

	maxResponseTime := c.Int("response-time-max")
	if maxResponseTime > 0 && int(stats.Max200Time/time.Millisecond) > maxResponseTime {
		log.Warnf("Max response time (%dms) was exceeded (limit: %dms).", int(stats.Max200Time/time.Millisecond), maxResponseTime)
		if exitCode == 0 {
			exitCode = c.Int("response-time-error")
		}
		return nil
	}

	if stats.Total == 0 && len(urls) > 0 {
		log.Warn("Crawled 0 URLs successfully, check logs for errors.")
	} else if exitCode == 0 {
		log.Debug("Crawl finished successfully.")
	}

	return nil
}

