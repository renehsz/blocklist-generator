package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"os"
	"path"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/gocolly/colly"
	_ "go.uber.org/automaxprocs"
	"golang.org/x/net/idna"
)

var (
	OutputDir = flag.String("output-dir", "out", "Directory to write blocklists to")
)

// TODO: If it's a www subdomain, consider also adding the apex domain

func cleanDomain(domain string) string {
	domain = strings.TrimSpace(domain)
	domain = strings.TrimPrefix(domain, "http://")
	domain = strings.TrimPrefix(domain, "https://")
	if strings.Contains(domain, "@") {
		domain = strings.Split(domain, "@")[1]
	}
	domain = strings.Split(domain, "/")[0]
	domain = strings.Split(domain, "#")[0]
	return domain
}

type Scraper interface {
	Scrape(domainsToBlock chan<- string) error
	GetBlocklistName() string
}

type WatchlistInternetScraper struct {
	TypesToScrape []string
}

func NewWatchlistInternetScraper(typesToScrape []string) *WatchlistInternetScraper {
	return &WatchlistInternetScraper{TypesToScrape: typesToScrape}
}

func (w *WatchlistInternetScraper) GetBlocklistName() string {
	return "watchlist_internet-" + strings.Join(w.TypesToScrape, "-")
}

func (w *WatchlistInternetScraper) Scrape(domainsToBlock chan<- string) error {
	c := colly.NewCollector(colly.AllowedDomains("www.watchlist-internet.at"))

	// Limit the number of threads started by colly to 1
	// TODO: Does this apply only to the current collector or to all collectors? Probably the former, but we would prefer the latter.
	c.Limit(&colly.LimitRule{
		DomainGlob:  "*",
		Parallelism: 1,
		Delay:       1500 * time.Millisecond,
	})
	c.SetRequestTimeout(45 * time.Second)

	c.AllowedDomains = []string{"www.watchlist-internet.at"}

	// Collect all domains for the blocklist
	c.OnHTML("div.row a[href].site-item__link", func(e *colly.HTMLElement) {
		domain := e.Text
		domainsToBlock <- cleanDomain(domain)
	})

	// Follow pagination links
	c.OnHTML("ul.pagination>li>a[href]", func(e *colly.HTMLElement) {
		nextPage := e.Attr("href")
		c.Visit(e.Request.AbsoluteURL(nextPage))
	})

	c.OnError(func(r *colly.Response, err error) {
		log.Printf("Error scraping watchlist-internet.at: %s", err)
		r.Request.Retry()
	})

	for _, typeToScrape := range w.TypesToScrape {
		if !strings.HasPrefix(typeToScrape, "liste-") {
			typeToScrape = "liste-" + typeToScrape
		}
		if !strings.HasSuffix(typeToScrape, "/") {
			typeToScrape = typeToScrape + "/"
		}
		//log.Printf("Scraping type %s", typeToScrape)
		c.Visit("https://www.watchlist-internet.at/" + typeToScrape)
	}

	close(domainsToBlock)
	return nil
}

const (
	// TODO: Keep this up-to-date
	FakeUserAgent = "Mozilla/5.0 (X11; Linux x86_64; rv:121.0) Gecko/20100101 Firefox/121.0"
)

type TrustedShopsScraper struct {
}

func NewTrustedShopsScraper() *TrustedShopsScraper {
	return &TrustedShopsScraper{}
}

func (t *TrustedShopsScraper) GetBlocklistName() string {
	return "trusted_shops-fakeshops"
}

func (t *TrustedShopsScraper) Scrape(domainsToBlock chan<- string) error {
	// Send URL-encoded POST request
	url := "https://www.trustedshops.de/wp-admin/admin-ajax.php"

	httpClient := &http.Client{
		Timeout: time.Second * 45,
	}
	const postsPerPage = 500
	offset := 0
	for {
		req, err := http.NewRequest("POST", url, strings.NewReader(
			"action=ts_query_fake_shops&query_vars[post_type]=fake_shop&query_vars[orderby]=date&query_vars[order]=desc"+
				"&query_vars[offset]=0"+strconv.Itoa(offset)+
				"&query_vars[posts_per_page]="+strconv.Itoa(postsPerPage)))

		if err != nil {
			return fmt.Errorf("Error creating HTTP request: %s", err)
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=UTF-8")
		req.Header.Set("X-Requested-With", "XMLHttpRequest")
		req.Header.Set("User-Agent", FakeUserAgent)
		req.Header.Set("Origin", "https://www.trustedshops.de")
		req.Header.Set("Referer", "https://www.trustedshops.de/fake-shops/")

		resp, err := httpClient.Do(req)
		if err != nil {
			return fmt.Errorf("Error sending HTTP request: %s", err)
		}

		if resp.StatusCode != 200 {
			return fmt.Errorf("HTTP request returned status code %d", resp.StatusCode)
		}

		type APIResponse struct {
			Result struct {
				Items []string `json:"items"`
				Total int      `json:"total"`
			} `json:"result"`
			Errors []string `json:"errors"`
		}

		apiResponse := APIResponse{}
		err = json.NewDecoder(resp.Body).Decode(&apiResponse)
		if err != nil {
			return fmt.Errorf("Error decoding JSON response: %s", err)
		}

		if len(apiResponse.Errors) > 0 {
			return fmt.Errorf("API returned errors: %s", strings.Join(apiResponse.Errors, ", "))
		}

		for _, item := range apiResponse.Result.Items {
			// The domain is between <h3> and </h3>
			domain := strings.Split(item, "<h3>")[1]
			domain = strings.Split(domain, "</h3>")[0]

			domainsToBlock <- cleanDomain(domain)
		}

		if len(apiResponse.Result.Items) < postsPerPage {
			break
		}

		offset += len(apiResponse.Result.Items)

		// Sleep for 15 to 30 seconds to avoid rate limiting
		time.Sleep(time.Duration(15+rand.Intn(15)) * time.Second)
	}

	close(domainsToBlock)
	return nil
}

func processDomainToBlock(d string) (string, error) {
	h, err := idna.Lookup.ToASCII(d)
	if err != nil {
		return "", fmt.Errorf("Error converting domain %s to ASCII: %s", d, err)
	}
	return strings.ToLower(h), nil
}

func writeBlocklist(blocklistName string, blocklist []string) error {
	if len(blocklist) == 0 {
		return errors.New("Blocklist is empty")
	}

	f, err := os.Create(path.Join(*OutputDir, blocklistName))
	if err != nil {
		return fmt.Errorf("Error creating blocklist file %s: %s", blocklistName, err)
	}
	defer f.Close()

	f.WriteString("# " + blocklistName + "\n")
	f.WriteString("# generated by blocklist-generator\n")
	f.WriteString("# Last updated: " + time.Now().Format(time.RFC3339) + "\n")
	for _, domain := range blocklist {
		_, err := f.WriteString(domain + "\n")
		if err != nil {
			return fmt.Errorf("Error writing domain %s to blocklist file %s: %s", domain, blocklistName, err)
		}
	}

	return nil
}

func runScraper(scraper Scraper, errchan chan<- error, domainsToBlock chan<- string) {
	err := scraper.Scrape(domainsToBlock)
	if err != nil {
		errchan <- fmt.Errorf("Error scraping for %s: %s", scraper.GetBlocklistName(), err)
	}
}

func Scrape(scrapers []Scraper) error {
	errs := []error{}
	errchan := make(chan error)

	blockChannels := make([]chan string, len(scrapers))
	for i, scraper := range scrapers {
		blockChannels[i] = make(chan string)
		go runScraper(scraper, errchan, blockChannels[i])
	}

	blocklists := make([][]string, len(scrapers))
	for i, _ := range blocklists {
		blocklists[i] = []string{}
	}

	cases := make([]reflect.SelectCase, len(blockChannels)+1)
	cases[0] = reflect.SelectCase{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(errchan)}
	for i, ch := range blockChannels {
		cases[i+1] = reflect.SelectCase{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(ch)}
	}

	for len(cases) > 1 {
		chosen, value, recvOk := reflect.Select(cases)

		// If the chosen channel is the error channel, we need to handle it separately
		if chosen == 0 {
			if !recvOk {
				panic("Error channel was closed")
			}

			// We received an error from the error channel
			if !value.IsNil() {
				errs = append(errs, value.Interface().(error))
			}
			continue
		}

		// Otherwise it's a channel of domains to block
		// If the channel is closed, remove it from the set of channels to receive from
		if !recvOk {
			cases = append(cases[:chosen], cases[chosen+1:]...)
			continue
		}

		// Process the value we received
		domainToBlock, err := processDomainToBlock(value.String())
		//log.Printf("Processing domain to block: %s", domainToBlock)
		if err != nil {
			errs = append(errs, fmt.Errorf("Error processing domain to block: %s", err))
			continue
		}
		blocklists[chosen-1] = append(blocklists[chosen-1], domainToBlock)
	}

	// De-duplicate the individual blocklists
	for i, _ := range blocklists {
		blocklists[i] = removeDuplicates(blocklists[i])
	}

	// Now write the individual blocklists to disk
	for i, blocklist := range blocklists {
		blocklistName := scrapers[i].GetBlocklistName()
		err := writeBlocklist(blocklistName, blocklist)
		if err != nil {
			errs = append(errs, fmt.Errorf("Error writing blocklist %s to disk: %s", blocklistName, err))
		}
	}

	// Merge the blocklists
	mergedBlocklist := []string{}
	for _, blocklist := range blocklists {
		mergedBlocklist = append(mergedBlocklist, blocklist...)
	}

	// De-duplicate the merged blocklist
	mergedBlocklist = removeDuplicates(mergedBlocklist)

	// Write the merged blocklist to disk
	err := writeBlocklist("combined_blocklist", mergedBlocklist)
	if err != nil {
		errs = append(errs, fmt.Errorf("Error writing merged blocklist to disk: %s", err))
	}

	return errors.Join(errs...)
}

// removeDuplicates removes duplicate elements from a slice
func removeDuplicates[T string | int](sliceList []T) []T {
	allKeys := make(map[T]bool)
	list := []T{}
	for _, item := range sliceList {
		if _, value := allKeys[item]; !value {
			allKeys[item] = true
			list = append(list, item)
		}
	}
	return list
}

func main() {
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "Usage: %s [options]\n", os.Args[0])
		flag.PrintDefaults()
	}
	flag.Parse()

	scrapers := []Scraper{}

	// Watchlist-Internet.at
	scraperTypes := []string{"betruegerischer-shops", "abo-fallen", "immobilienagenturen", "urlaubsbuchung", "handwerksdienste", "speditionen", "jobangebote", "finanzbetrug"}
	for _, scraperType := range scraperTypes {
		scrapers = append(scrapers, NewWatchlistInternetScraper([]string{scraperType}))
	}

	// TrustedShops.de Fakeshops
	scrapers = append(scrapers, NewTrustedShopsScraper())

	if err := Scrape(scrapers); err != nil {
		log.Fatal(err)
	}
}
