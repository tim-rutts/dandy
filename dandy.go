package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/PuerkitoBio/goquery"
)

const (
	MinYear  = 1922
	MaxYear  = 2008
	hrefAttr = "href"
	altAttr  = "alt"
)

var (
	yearPH  = "YYYY"
	yearUrl = fmt.Sprintf("https://croco.uno/year/%v", yearPH)
)

type Magazine struct {
	Year Year
	Addr string
	Name string
	Err  error
}

type Year int

func (y *Year) String() string {
	return strconv.Itoa(int(*y))
}

func (y *Year) Addr() string {
	return strings.ReplaceAll(yearUrl, yearPH, y.String())
}

type YearPage struct {
	year    Year
	content io.ReadCloser
	closed  bool
	err     error
}

func (y *YearPage) Close() {
	if y.closed {
		return
	}
	if y.content == nil {
		y.closed = true
		return
	}
	_ = y.content.Close()
	y.closed = true
}

type Downloader interface {
	Run(ctx context.Context) <-chan struct{}
}

type dandyDownloader struct {
	from, to     int
	count        int
	verbose      bool
	done         chan struct{}
	output       string
	started      time.Time
	totYears     *int32
	totYearsDone *int32
	totMags      *int32
	totMagsDone  *int32
}

func newDandyDownloader(from, to int, verbose bool, output string) *dandyDownloader {
	return &dandyDownloader{
		from:         from,
		to:           to,
		count:        from - to + 1,
		verbose:      verbose,
		output:       output,
		totYears:     new(int32),
		totYearsDone: new(int32),
		totMags:      new(int32),
		totMagsDone:  new(int32),
	}
}

func NewDownloader(from, to, count int, verbose bool, output string) (Downloader, error) {
	f, t, err := calcYearsRange(from, to, count)
	if err != nil {
		return nil, err
	}

	exists, err := pathExists(output)
	if err != nil {
		return nil, err
	}
	if !exists {
		err = createDir(output)
		if err != nil {
			return nil, err
		}
	}

	return newDandyDownloader(f, t, verbose, output), nil
}

func (d *dandyDownloader) Run(ctx context.Context) <-chan struct{} {
	d.done = make(chan struct{})
	go func() {
		defer func() {
			fatal := recover()
			d.reportFinished(fatal)
			close(d.done)
		}()
		d.reportStarted()
		d.run(ctx)
	}()
	return d.done
}

func (d *dandyDownloader) run(ctx context.Context) {
	years := d.genYears(ctx)
	pages := d.downloadYearPages(ctx, years)
	links := d.parseYearPages(ctx, pages)
	d.reportMagazineLinks(ctx, links)

	panic("not implemented")
}

func (d *dandyDownloader) downloadYearPages(ctx context.Context, years <-chan Year) <-chan *YearPage {
	c := make(chan *YearPage)
	go func() {
		defer close(c)
		for year := range years {
			page := d.downloadYearPage(ctx, year)
			select {
			case c <- page:
				d.incYearProcessed()
				d.reportYearPage(year, page.err)
				break
			case <-ctx.Done():
				return
			}
		}
	}()
	return c
}

func (d *dandyDownloader) downloadYearPage(ctx context.Context, year Year) *YearPage {
	errYear := func(err error) *YearPage { return &YearPage{year: year, err: err} }
	rq, err := http.NewRequestWithContext(ctx, http.MethodGet, year.Addr(), nil)
	if err != nil {
		return errYear(err)
	}

	resp, err := http.DefaultClient.Do(rq)
	if err != nil {
		return errYear(err)
	}

	if resp.StatusCode != http.StatusOK {
		return errYear(fmt.Errorf("status code %v", resp.StatusCode))
	}
	return &YearPage{year: year, content: resp.Body}
}

func (d *dandyDownloader) parseYearPages(ctx context.Context, pages <-chan *YearPage) <-chan *Magazine {
	c := make(chan *Magazine)
	go func() {
		defer close(c)
		for page := range pages {
			links := d.parseYearPage(page)
			for _, link := range links {
				select {
				case c <- link:
					d.incMag()
					break
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return c
}

func (d *dandyDownloader) parseYearPage(page *YearPage) []*Magazine {
	errMag := func(y Year, err error) []*Magazine { return []*Magazine{{Year: y, Err: err}} }
	if page.err != nil {
		return errMag(page.year, page.err)
	}
	defer page.Close()

	doc, err := goquery.NewDocumentFromReader(page.content)
	if err != nil {
		return errMag(page.year, err)
	}
	set := doc.Find("div.card a")
	mgs := make([]*Magazine, 0, len(set.Nodes))
	for _, node := range set.Nodes {
		if node == nil {
			continue
		}

		nd := goquery.NewDocumentFromNode(node)
		link, ok := nd.Attr(hrefAttr)
		if !ok {
			continue
		}
		if _, err = url.ParseRequestURI(link); err != nil {
			continue
		}
		altTxt, _ := nd.Find("img").First().Attr(altAttr)

		mg := &Magazine{
			Year: page.year,
			Addr: link,
			Name: altTxt,
		}
		mgs = append(mgs, mg)
	}

	if len(mgs) == 0 {
		return errMag(page.year, errors.New("no links found"))
	}
	return mgs
}

func (d *dandyDownloader) genYears(ctx context.Context) <-chan Year {
	c := make(chan Year)
	go func() {
		defer close(c)
		for i := d.from; i <= d.to; i++ {
			select {
			case c <- Year(i):
				d.incYear()
				break
			case <-ctx.Done():
				return
			}
		}
	}()
	return c
}

func (d *dandyDownloader) reportMagazineLinks(ctx context.Context, links <-chan *Magazine) {
	for link := range links {
		select {
		case <-ctx.Done():
			return
		default:
			d.report(fmt.Sprintf("%v magazine %v", time.Now(), link))
			time.Sleep(200 * time.Millisecond)
		}
	}
}

func (d *dandyDownloader) reportYearPage(y Year, err error) {
	if err != nil {
		d.report(fmt.Sprintf("%v cannot download %v becaues of %v", time.Now(), y, err))
		return
	}
	d.report(fmt.Sprintf("%v downloaded %v year page", time.Now(), y))
}

func (d *dandyDownloader) reportStarted() {
	d.started = time.Now()
	d.report(fmt.Sprintf("started working for years from %v to %v (total year(s) %v)", d.from, d.to, d.count))
}

func (d *dandyDownloader) reportFinished(fatalErr interface{}) {
	if fatalErr != nil {
		d.report(fmt.Sprintf("finished working for %v with fatal err %v stat %v", time.Since(d.started), fatalErr, d.stat()))
		return
	}
	d.report(fmt.Sprintf("finished working for %v stat %v", time.Since(d.started), d.stat()))
}

func (d *dandyDownloader) report(data interface{}) {
	if !d.verbose {
		return
	}
	fmt.Println(data)
}

func (d *dandyDownloader) stat() string {
	return fmt.Sprintf("total years: %v processed: %v total magazines %v processed %v", *d.totYears, *d.totYearsDone, *d.totMags, *d.totMagsDone)
}

func (d *dandyDownloader) incYear() {
	atomic.AddInt32(d.totYears, 1)
}

func (d *dandyDownloader) incYearProcessed() {
	atomic.AddInt32(d.totYearsDone, 1)
}

func (d *dandyDownloader) incMag() {
	atomic.AddInt32(d.totMags, 1)
}

func (d *dandyDownloader) incMagProcessed() {
	atomic.AddInt32(d.totMagsDone, 1)
}

func calcYearsRange(f, t, c int) (from, to int, err error) {
	if f == 0 {
		err = errors.New("from year is nil or zero")
		return
	}
	from = f

	if t == 0 {
		to = from
		if c != 0 {
			to = from + c - 1
		}
	} else {
		to = t
	}

	if from < MinYear {
		err = fmt.Errorf("year %v is less than allowed %v", from, MinYear)
		return
	}
	if to > MaxYear {
		err = fmt.Errorf("year %v is greater than allowed %v", to, MaxYear)
		return
	}
	if to < from {
		err = fmt.Errorf("year %v is less than allowed %v", to, from)
		return
	}
	return
}

func pathExists(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

func createDir(path string) error {
	return os.Mkdir(path, 0777)
}
