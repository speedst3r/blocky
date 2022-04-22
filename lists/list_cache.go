package lists

//go:generate go-enum -f=$GOFILE --marshal --names
import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/0xERR0R/blocky/cache/stringcache"

	"github.com/avast/retry-go/v4"

	"github.com/hako/durafmt"

	"github.com/hashicorp/go-multierror"

	"github.com/0xERR0R/blocky/evt"
	"github.com/0xERR0R/blocky/log"
	"github.com/sirupsen/logrus"
)

// ListCacheType represents the type of cached list ENUM(
// blacklist // is a list with blocked domains
// whitelist // is a list with whitelisted domains / IPs
// )
type ListCacheType int

// Matcher checks if a domain is in a list
type Matcher interface {
	// Match matches passed domain name against cached list entries
	Match(domain string, groupsToCheck []string) (found bool, group string)

	// Configuration returns current configuration and stats
	Configuration() []string
}

// ListCache generic cache of strings divided in groups
type ListCache struct {
	groupCaches map[string]stringcache.StringCache
	lock        sync.RWMutex

	groupToLinks     map[string][]string
	refreshPeriod    time.Duration
	downloadTimeout  time.Duration
	downloadAttempts int
	downloadCooldown time.Duration
	httpTransport    *http.Transport
	listType         ListCacheType
}

// Configuration returns current configuration and stats
func (b *ListCache) Configuration() (result []string) {
	if b.refreshPeriod > 0 {
		result = append(result, fmt.Sprintf("refresh period: %s", durafmt.Parse(b.refreshPeriod)))
	} else {
		result = append(result, "refresh: disabled")
	}

	result = append(result, "group links:")
	for group, links := range b.groupToLinks {
		result = append(result, fmt.Sprintf("  %s:", group))

		for _, link := range links {
			if strings.Contains(link, "\n") {
				link = "[INLINE DEFINITION]"
			}

			result = append(result, fmt.Sprintf("   - %s", link))
		}
	}

	result = append(result, "group caches:")

	var total int

	for group, cache := range b.groupCaches {
		result = append(result, fmt.Sprintf("  %s: %d entries", group, cache.ElementCount()))
		total += cache.ElementCount()
	}

	result = append(result, fmt.Sprintf("  TOTAL: %d entries", total))

	return result
}

// NewListCache creates new list instance
func NewListCache(t ListCacheType, groupToLinks map[string][]string, refreshPeriod time.Duration,
	downloadTimeout time.Duration, downloadAttempts int,
	downloadCooldown time.Duration, httpTransport *http.Transport) (*ListCache, error) {
	groupCaches := make(map[string]stringcache.StringCache)

	b := &ListCache{
		groupToLinks:     groupToLinks,
		groupCaches:      groupCaches,
		refreshPeriod:    refreshPeriod,
		downloadTimeout:  downloadTimeout,
		downloadAttempts: downloadAttempts,
		downloadCooldown: downloadCooldown,
		httpTransport:    httpTransport,
		listType:         t,
	}
	initError := b.refresh(true)

	if initError == nil {
		go periodicUpdate(b)
	}

	return b, initError
}

// periodicUpdate triggers periodical refresh (and download) of list entries
func periodicUpdate(cache *ListCache) {
	if cache.refreshPeriod > 0 {
		ticker := time.NewTicker(cache.refreshPeriod)
		defer ticker.Stop()

		for {
			<-ticker.C
			cache.Refresh()
		}
	}
}

func logger() *logrus.Entry {
	return log.PrefixedLog("list_cache")
}

type groupCache struct {
	cache []string
	err   error
}

// downloads and reads files with domain names and creates cache for them
func (b *ListCache) createCacheForGroup(links []string) (stringcache.StringCache, error) {
	var wg sync.WaitGroup

	var err error

	c := make(chan groupCache, len(links))
	// loop over links (http/local) or inline definitions
	for _, link := range links {
		wg.Add(1)

		go b.processFile(link, c, &wg)
	}

	wg.Wait()

	factory := stringcache.NewChainedCacheFactory()

Loop:
	for {
		select {
		case res := <-c:
			if res.err != nil {
				err = multierror.Append(err, res.err)
			}
			if res.cache == nil {
				return nil, err
			}
			for _, entry := range res.cache {
				factory.AddEntry(entry)
			}
		default:
			close(c)
			break Loop
		}
	}

	return factory.Create(), err
}

// Match matches passed domain name against cached list entries
func (b *ListCache) Match(domain string, groupsToCheck []string) (found bool, group string) {
	b.lock.RLock()
	defer b.lock.RUnlock()

	for _, g := range groupsToCheck {
		if c, ok := b.groupCaches[g]; ok && c.Contains(domain) {
			return true, g
		}
	}

	return false, ""
}

// Refresh triggers the refresh of a list
func (b *ListCache) Refresh() {
	_ = b.refresh(false)
}
func (b *ListCache) refresh(init bool) error {
	var err error

	for group, links := range b.groupToLinks {
		cacheForGroup, e := b.createCacheForGroup(links)
		if e != nil {
			err = multierror.Append(err, multierror.Prefix(e, fmt.Sprintf("can't create cache group '%s':", group)))
		}

		if cacheForGroup != nil {
			b.lock.Lock()
			b.groupCaches[group] = cacheForGroup
			b.lock.Unlock()
		} else {
			if init {
				msg := "Populating group cache failed for group " + group
				logger().Warn(msg)
			} else {
				logger().Warn("Populating of group cache failed, leaving items from last successful download in cache")
			}
		}

		if b.groupCaches[group] != nil {
			evt.Bus().Publish(evt.BlockingCacheGroupChanged, b.listType, group, b.groupCaches[group].ElementCount())

			logger().WithFields(logrus.Fields{
				"group":       group,
				"total_count": b.groupCaches[group].ElementCount(),
			}).Info("group import finished")
		}
	}

	return err
}

func (b *ListCache) downloadFile(link string) (io.ReadCloser, error) {
	client := http.Client{
		Timeout:   b.downloadTimeout,
		Transport: b.httpTransport,
	}

	var resp *http.Response

	logger().WithField("link", link).Info("starting download")

	var body io.ReadCloser

	err := retry.Do(
		func() error {
			var err error
			//nolint:bodyclose
			if resp, err = client.Get(link); err == nil {
				if resp.StatusCode == http.StatusOK {
					body = resp.Body
					return nil
				}

				_ = resp.Body.Close()

				return fmt.Errorf("got status code %d", resp.StatusCode)
			}
			return err
		},
		retry.Attempts(uint(b.downloadAttempts)),
		retry.DelayType(retry.FixedDelay),
		retry.Delay(b.downloadCooldown),
		retry.LastErrorOnly(true),
		retry.OnRetry(func(n uint, err error) {
			var netErr net.Error

			var dnsErr *net.DNSError

			logger := logger().WithField("link", link).WithField("attempt",
				fmt.Sprintf("%d/%d", n+1, b.downloadAttempts))

			switch {
			case errors.As(err, &netErr) && (netErr.Timeout() || netErr.Temporary()):
				logger.Warnf("Temporary network err / Timeout occurred: %s", netErr)
			case errors.As(err, &dnsErr):
				logger.Warnf("Name resolution err: %s", dnsErr.Err)
			default:
				logger.Warnf("Can't download file: %s", err)
			}

			evt.Bus().Publish(evt.CachingFailedDownloadChanged, link)
		}))

	return body, err
}

func readFile(file string) (io.ReadCloser, error) {
	logger().WithField("file", file).Info("starting processing of file")
	file = strings.TrimPrefix(file, "file://")

	return os.Open(file)
}

// downloads file (or reads local file) and writes file content as string array in the channel
func (b *ListCache) processFile(link string, ch chan<- groupCache, wg *sync.WaitGroup) {
	defer wg.Done()

	result := groupCache{
		cache: []string{},
	}

	var r io.ReadCloser

	var err error

	r, err = b.getLinkReader(link)

	if err != nil {
		logger().Warn("err during file processing: ", err)
		result.err = multierror.Append(result.err, err)

		var netErr net.Error

		if errors.As(err, &netErr) && (netErr.Timeout() || netErr.Temporary()) {
			// put nil to indicate the temporary err
			result.cache = nil
		}
		ch <- result

		return
	}
	defer r.Close()

	var count int

	scanner := bufio.NewScanner(r)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		// skip comments
		if line := processLine(line); line != "" {
			result.cache = append(result.cache, line)

			count++
		}
	}

	if err := scanner.Err(); err != nil {
		logger().Warn("can't parse file: ", err)
	} else {
		logger().WithFields(logrus.Fields{
			"source": link,
			"count":  count,
		}).Info("file imported")
	}
	ch <- result
}

func (b *ListCache) getLinkReader(link string) (r io.ReadCloser, err error) {
	switch {
	// link contains a line break -> this is inline list definition in YAML (with literal style Block Scalar)
	case strings.ContainsAny(link, "\n"):
		r = io.NopCloser(strings.NewReader(link))
	// link is http(s) -> download it
	case strings.HasPrefix(link, "http"):
		r, err = b.downloadFile(link)
	// probably path to a local file
	default:
		r, err = readFile(link)
	}

	return
}

// return only first column (see hosts format)
func processLine(line string) string {
	if strings.HasPrefix(line, "#") {
		return ""
	}

	parts := strings.Fields(line)

	if len(parts) > 0 {
		host := parts[len(parts)-1]

		ip := net.ParseIP(host)
		if ip != nil {
			return ip.String()
		}

		return strings.TrimSpace(strings.ToLower(host))
	}

	return ""
}
