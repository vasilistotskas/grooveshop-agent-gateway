//go:build feedmem

// A measurement, not a regression test: the memory one feed generation
// takes for a synthetic catalogue. It runs only with the feedmem build
// tag, one case per process, so each case starts from a clean heap and
// Sys is not inherited from the case before:
//
//	FEEDMEM_PRODUCTS=10000 FEEDMEM_SWEEPS=2 \
//	  go test -tags feedmem -run TestFeedMemory -count=1 -v ./internal/feeds/
//
// It runs the real generation through Service.refresh (generationSlots,
// fetchAllProducts, the writers, the gzip streams, store), FEEDMEM_SWEEPS
// stores at once, and samples runtime.ReadMemStats every millisecond.
// FEEDMEM_MODE=fetchonly sweeps the catalogue and maps every row without
// writing any document: the floor of a generation. GOGC and GOMEMLIMIT
// are read from the environment as the runtime always does; production
// runs GOMEMLIMIT=90MiB (grooveshop-infrastructure,
// manifests/app-constructs/grooveshop/base/agent-gateway.yaml).
//
// Django is an in-process httptest server that renders each page on
// request, so its rendering is counted in this heap and not in a pod's.
// Redis is a client hook that answers every command with no server and
// keeps only what it was asked to SET: the sizes and the ETag, which
// tell whether a change still renders the same documents.
package feeds

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/obs"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/storefront"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/tenant"
)

const feedmemImageTpl = "https://{assets_host}/media_stream-image/{path}" +
	"/1000/1000/contain/center/FFFFFF/5/85.jpeg"

var greekWords = strings.Fields(`και το της με για από στο που είναι
	προϊόν ποιότητα υλικό σχεδιασμός άνεση αντοχή χρώμα μέγεθος ύφασμα
	βαμβάκι δέρμα ξύλο μέταλλο κατασκευή καθημερινή χρήση εύκολο καθαρισμό
	ιδανικό δώρο οικογένεια σπίτι κουζίνα γραφείο ταξίδι εξαιρετική
	φινίρισμα λεπτομέρειες χειροποίητο ελληνικό εργαστήριο παράδοση
	σύγχρονο κλασικό ανθεκτικό ελαφρύ μαλακό ζεστό δροσερό καλοκαίρι
	χειμώνα συσκευασία εγγύηση οδηγίες φροντίδας`)

var englishWords = strings.Fields(`the and with for from this product
	quality material design comfort durable colour size fabric cotton
	leather wood metal everyday use easy care ideal gift family home
	kitchen office travel finish handmade workshop modern classic light
	soft warm summer winter packaging warranty instructions`)

func words(r *rand.Rand, list []string, runes int) string {
	var b strings.Builder
	n := 0
	for n < runes {
		w := list[r.IntN(len(list))]
		if b.Len() > 0 {
			b.WriteByte(' ')
			n++
		}
		b.WriteString(w)
		n += len([]rune(w))
	}
	return b.String()
}

// htmlDescription is rich text as the admin's editor stores it:
// paragraphs of about 300 characters, some bold, one list.
func htmlDescription(r *rand.Rand, list []string, runes int) string {
	var b strings.Builder
	for n := 0; n < runes; n += 300 {
		b.WriteString("<p>")
		if r.IntN(3) == 0 {
			b.WriteString("<strong>" + words(r, list, 30) + "</strong> ")
		}
		b.WriteString(words(r, list, 270))
		b.WriteString("</p>")
	}
	b.WriteString("<ul><li>" + words(r, list, 40) + "</li><li>" +
		words(r, list, 40) + "</li></ul>")
	return b.String()
}

type fakeTr struct {
	Name           string `json:"name"`
	Description    string `json:"description"`
	SeoTitle       string `json:"seoTitle"`
	SeoDescription string `json:"seoDescription"`
}

type fakeAttr struct {
	AttributeID   int    `json:"attributeId"`
	AttributeName string `json:"attributeName"`
	ValueID       int    `json:"valueId"`
	Value         string `json:"value"`
	IsVariant     bool   `json:"isVariant"`
}

// fakeProduct is a /product/feed row with the fields ProductFeedSerializer
// sends, consumed or not, since the client decodes the whole page.
type fakeProduct struct {
	ID                       int64             `json:"id"`
	Translations             map[string]fakeTr `json:"translations"`
	Slug                     string            `json:"slug"`
	Category                 int64             `json:"category"`
	VariantGroup             *int64            `json:"variantGroup"`
	VariantGroupTitle        *string           `json:"variantGroupTitle"`
	Brand                    *int64            `json:"brand"`
	BrandName                *string           `json:"brandName"`
	Gtin                     string            `json:"gtin"`
	Mpn                      string            `json:"mpn"`
	Price                    float64           `json:"price"`
	Vat                      int               `json:"vat"`
	ViewCount                int               `json:"viewCount"`
	Stock                    int               `json:"stock"`
	LowStockThreshold        int               `json:"lowStockThreshold"`
	Active                   bool              `json:"active"`
	Weight                   map[string]any    `json:"weight"`
	DiscountPercent          float64           `json:"discountPercent"`
	VatPercent               float64           `json:"vatPercent"`
	FinalPrice               float64           `json:"finalPrice"`
	EffectivePrice           float64           `json:"effectivePrice"`
	PriorPrice               *float64          `json:"priorPrice"`
	AnnouncedDiscountPercent *float64          `json:"announcedDiscountPercent"`
	MainImagePath            string            `json:"mainImagePath"`
	ImagePaths               []string          `json:"imagePaths"`
	Attributes               []fakeAttr        `json:"attributes"`
	ReviewAverage            float64           `json:"reviewAverage"`
	ReviewCount              int               `json:"reviewCount"`
	LikesCount               int               `json:"likesCount"`
	CreatedAt                string            `json:"createdAt"`
	UpdatedAt                string            `json:"updatedAt"`
	UUID                     string            `json:"uuid"`
	Highlights               []string          `json:"highlights"`
}

// fakeProductRow is product id of the synthetic catalogue: Greek and
// English text, descriptions of about 1,500 characters and every tenth
// one of 12,000 (cut to 5,000 in the feed), five images and every tenth
// product fourteen, six in ten products in variant groups of four with
// two variant axes, eight specifications and four highlights.
func fakeProductRow(id int64) fakeProduct {
	r := rand.New(rand.NewPCG(uint64(id), 127))
	descRunes := 1500
	images := 5
	if id%10 == 0 {
		descRunes = 12000
		images = 14
	}
	slug := fmt.Sprintf("product-%d-%s", id,
		strings.ReplaceAll(words(r, englishWords, 25), " ", "-"))
	paths := make([]string, images)
	for i := range paths {
		paths[i] = fmt.Sprintf("uploads/products/%d/%s-%d.jpg", id, slug, i)
	}
	brand := "Εργαστήριο " + words(r, greekWords, 12)
	brandID := id % 40
	p := fakeProduct{
		ID: id,
		Translations: map[string]fakeTr{
			"el": {
				Name:           words(r, greekWords, 60),
				Description:    htmlDescription(r, greekWords, descRunes),
				SeoTitle:       words(r, greekWords, 60),
				SeoDescription: words(r, greekWords, 150),
			},
			"en": {
				Name:           words(r, englishWords, 60),
				Description:    htmlDescription(r, englishWords, descRunes),
				SeoTitle:       words(r, englishWords, 60),
				SeoDescription: words(r, englishWords, 150),
			},
		},
		Slug:              slug,
		Category:          1 + id%20,
		Brand:             &brandID,
		BrandName:         &brand,
		Gtin:              fmt.Sprintf("520%010d", id),
		Mpn:               fmt.Sprintf("MPN-%08d", id),
		Price:             24.19,
		Vat:               24,
		ViewCount:         int(id * 7 % 5000),
		Stock:             int(id % 7),
		LowStockThreshold: 5,
		Active:            true,
		Weight:            map[string]any{"unit": "g", "value": 350.0},
		VatPercent:        24,
		FinalPrice:        29.99,
		EffectivePrice:    29.99,
		MainImagePath:     paths[0],
		ImagePaths:        paths,
		ReviewAverage:     8.5,
		ReviewCount:       12,
		LikesCount:        3,
		CreatedAt:         "2026-03-01T10:00:00.000000+02:00",
		UpdatedAt:         "2026-10-01T10:00:00.000000+03:00",
		UUID:              fmt.Sprintf("00000000-0000-4000-8000-%012d", id),
	}
	if id%5 == 0 {
		prior, pct := 34.99, 14.0
		p.PriorPrice, p.AnnouncedDiscountPercent = &prior, &pct
		p.EffectivePrice = 29.99
	}
	if id%10 < 6 {
		group := id / 4
		title := words(r, greekWords, 50)
		p.VariantGroup, p.VariantGroupTitle = &group, &title
		p.Attributes = append(p.Attributes,
			fakeAttr{1, "Χρώμα", int(id), words(r, greekWords, 8), true},
			fakeAttr{2, "Μέγεθος", int(id) + 1, "XL", true})
	}
	for i := range 8 {
		p.Attributes = append(p.Attributes, fakeAttr{
			10 + i, words(r, greekWords, 14), 100 + i,
			words(r, greekWords, 30), false,
		})
	}
	for range 4 {
		p.Highlights = append(p.Highlights, words(r, greekWords, 90))
	}
	return p
}

// feedmemDjango serves categories and a catalogue of products rows,
// rendered per request so the fake holds no catalogue of its own.
func feedmemDjango(t testing.TB, products int) *django.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if strings.HasSuffix(r.URL.Path, "/product/category/all") {
				cats := make([]map[string]any, 20)
				for i := range cats {
					cats[i] = map[string]any{
						"id": i + 1, "slug": fmt.Sprintf("cat-%d", i+1),
						"translations": map[string]any{"el": map[string]any{
							"name": "Κατηγορία " + strconv.Itoa(i+1)}},
						"active": true, "parent": nil, "level": 0, "treeId": i + 1,
					}
				}
				_ = json.NewEncoder(w).Encode(cats)
				return
			}
			page, _ := strconv.Atoi(r.URL.Query().Get("page"))
			size, _ := strconv.Atoi(r.URL.Query().Get("pageSize"))
			total := (products + size - 1) / size
			first := (page-1)*size + 1
			last := min(page*size, products)
			rows := make([]fakeProduct, 0, size)
			for id := first; id <= last; id++ {
				rows = append(rows, fakeProductRow(int64(id)))
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"links": map[string]any{"next": nil, "previous": nil},
				"count": products, "totalPages": total, "pageSize": size,
				"page": page, "results": rows,
			})
		}))
	t.Cleanup(srv.Close)
	return django.New(srv.URL+"/api/v1", "api.example.test", "secret",
		time.Minute, discard, obs.NewMetrics())
}

type peak struct {
	heapInuse, heapAlloc, sys uint64
}

// sample reads MemStats every interval until stop is closed.
func sample(interval time.Duration, stop <-chan struct{}) <-chan peak {
	out := make(chan peak, 1)
	go func() {
		var p peak
		var m runtime.MemStats
		tick := time.NewTicker(interval)
		defer tick.Stop()
		for {
			runtime.ReadMemStats(&m)
			p.heapInuse = max(p.heapInuse, m.HeapInuse)
			p.heapAlloc = max(p.heapAlloc, m.HeapAlloc)
			p.sys = max(p.sys, m.Sys)
			select {
			case <-stop:
				out <- p
				return
			case <-tick.C:
			}
		}
	}()
	return out
}

func mib(b uint64) string { return fmt.Sprintf("%.1f", float64(b)/(1<<20)) }

func feedmemTenant(i int) *tenant.Tenant {
	return &tenant.Tenant{
		TenantConfig: django.TenantConfig{
			SchemaName:      fmt.Sprintf("store%d", i),
			StoreName:       "Κατάστημα δοκιμής",
			DefaultLocale:   "el",
			DefaultCurrency: "EUR",
		},
		Domain: fmt.Sprintf("shop%d.example.test", i),
	}
}

func envInt(t *testing.T, name string, def int) int {
	v := os.Getenv(name)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	require.NoError(t, err)
	return n
}

// stored records what the generation asked Redis to SET: each blob's
// length and each feed's metadata.
type stored struct {
	mu   sync.Mutex
	gz   map[string]int
	meta map[string]Meta
}

func (s *stored) DialHook(next redis.DialHook) redis.DialHook { return next }

func (s *stored) ProcessHook(redis.ProcessHook) redis.ProcessHook {
	return func(_ context.Context, cmd redis.Cmder) error { return s.run(cmd) }
}

func (s *stored) ProcessPipelineHook(
	redis.ProcessPipelineHook,
) redis.ProcessPipelineHook {
	return func(_ context.Context, cmds []redis.Cmder) error {
		for _, cmd := range cmds {
			if err := s.run(cmd); err != nil {
				return err
			}
		}
		return nil
	}
}

func (s *stored) run(cmd redis.Cmder) error {
	args := cmd.Args()
	switch cmd.Name() {
	case "get":
		cmd.SetErr(redis.Nil)
		return redis.Nil
	case "set":
		key, _ := args[1].(string)
		val, _ := args[2].([]byte)
		s.mu.Lock()
		defer s.mu.Unlock()
		if data, ok := strings.CutSuffix(key, ":meta"); ok {
			var m Meta
			if err := json.Unmarshal(val, &m); err != nil {
				return err
			}
			s.meta[data] = m
		} else {
			s.gz[key] = len(val)
		}
	}
	return nil
}

func TestFeedMemory(t *testing.T) {
	products := envInt(t, "FEEDMEM_PRODUCTS", 1000)
	sweeps := envInt(t, "FEEDMEM_SWEEPS", 1)
	require.LessOrEqual(t, products, maxPages*pageSize)
	dj := feedmemDjango(t, products)
	t.Logf("GOGC=%q GOMEMLIMIT=%q (runtime limit %d) GOMAXPROCS=%d %s",
		os.Getenv("GOGC"), os.Getenv("GOMEMLIMIT"),
		debug.SetMemoryLimit(-1), runtime.GOMAXPROCS(0), runtime.Version())
	if os.Getenv("FEEDMEM_MODE") == "fetchonly" {
		fetchOnly(t, dj, products, sweeps)
		return
	}

	rec := &stored{gz: map[string]int{}, meta: map[string]Meta{}}
	rdb := redis.NewClient(&redis.Options{
		Addr: "127.0.0.1:1", DialTimeout: 20 * time.Millisecond,
		MaxRetries: -1, PoolSize: 8,
	})
	rdb.AddHook(rec)
	s := NewService(dj, rdb, discard, obs.NewMetrics(), feedmemImageTpl,
		"assets.example.test", time.Hour, 24*time.Hour, "feedmem",
		storefront.NewSite("el"))

	before, stop, peaks := startSampling()
	start := time.Now()
	var wg sync.WaitGroup
	errs := make([]error, sweeps)
	for i := range sweeps {
		wg.Go(func() {
			res := <-s.refresh(context.Background(), feedmemTenant(i))
			errs[i] = res.Err
		})
	}
	wg.Wait()
	elapsed := time.Since(start)
	close(stop)
	p := <-peaks
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	for _, err := range errs {
		require.NoError(t, err)
	}
	rss, acp := dataKey("store0", formatRSS), dataKey("store0", formatACP)
	fmt.Printf("FEEDMEM products=%d sweeps=%d gogc=%q gomemlimit=%q "+
		"duration_s=%.2f baseline_heapinuse_mib=%s peak_heapinuse_mib=%s "+
		"peak_heapalloc_mib=%s peak_sys_mib=%s totalalloc_mib=%s numgc=%d "+
		"rss_raw=%d rss_gz=%d rss_etag=%s acp_raw=%d acp_gz=%d acp_etag=%s\n",
		products, sweeps, os.Getenv("GOGC"), os.Getenv("GOMEMLIMIT"),
		elapsed.Seconds(), mib(before.HeapInuse), mib(p.heapInuse),
		mib(p.heapAlloc), mib(p.sys),
		mib(after.TotalAlloc-before.TotalAlloc), after.NumGC-before.NumGC,
		rec.meta[rss].Size, rec.gz[rss], rec.meta[rss].ETag,
		rec.meta[acp].Size, rec.gz[acp], rec.meta[acp].ETag)
}

// startSampling collects garbage, then samples MemStats every
// millisecond until stop is closed; before is the heap it starts from.
func startSampling() (
	before runtime.MemStats, stop chan struct{}, peaks <-chan peak,
) {
	runtime.GC()
	runtime.ReadMemStats(&before)
	stop = make(chan struct{})
	return before, stop, sample(time.Millisecond, stop)
}

// fetchOnly sweeps the catalogue and maps every row to a feedItem, but
// writes no document: the floor a generation cannot go below (the fetch
// window, the mapping, and this process's fake Django).
func fetchOnly(t *testing.T, dj *django.Client, products, sweeps int) {
	fctx := &feedContext{
		StoreName:  "Κατάστημα δοκιμής",
		Domain:     "shop.example.test",
		Links:      storefront.NewSite("el").Links("shop.example.test", "el"),
		AssetsHost: "assets.example.test", Schema: "store0",
		Currency: "EUR", Locale: "el", ImageURLTemplate: feedmemImageTpl,
		CategoryNames: map[int64]string{},
	}
	before, stop, peaks := startSampling()
	start := time.Now()
	var wg sync.WaitGroup
	errs := make([]error, sweeps)
	for i := range sweeps {
		wg.Go(func() {
			_, _, errs[i] = fetchAllProducts(context.Background(), dj, discard,
				"shop.example.test", "el", func(p django.ProductFeed) error {
					_, err := newFeedItem(&p, fctx)
					return err
				})
		})
	}
	wg.Wait()
	elapsed := time.Since(start)
	close(stop)
	p := <-peaks
	for _, err := range errs {
		require.NoError(t, err)
	}
	fmt.Printf("FEEDMEM-FETCHONLY products=%d sweeps=%d gomemlimit=%q "+
		"duration_s=%.2f baseline_heapinuse_mib=%s peak_heapinuse_mib=%s "+
		"peak_sys_mib=%s\n",
		products, sweeps, os.Getenv("GOMEMLIMIT"), elapsed.Seconds(),
		mib(before.HeapInuse), mib(p.heapInuse), mib(p.sys))
}
