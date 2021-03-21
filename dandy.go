package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
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
	ext      = "pdf"
)

var (
	yearPH  = "YYYY"
	yearUrl = fmt.Sprintf("https://croco.uno/year/%v", yearPH)
)

type Magazine struct {
	Year     Year
	Addr     string
	Name     string
	Err      error
	Size     int64
	Filepath string
}

func (m *Magazine) String() string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Year %v Name %q Size %v Filepath %v\n", m.Year, m.Name, m.Size, m.Filepath))
	sb.WriteString(fmt.Sprintf("Addr %q", m.Addr))
	if m.Err != nil {
		sb.WriteString(fmt.Sprintf("\nError %v", m.Err))
	}
	return sb.String()
}

func (m *Magazine) Filename() (string, error) {
	if len(m.Name) > 0 {
		return m.filename(m.Name), nil
	}

	if len(m.Addr) == 0 {
		return "", errors.New("addr is empty")
	}

	_, fn := path.Split(m.Addr)
	if len(fn) == 0 {
		return "", errors.New("cannot extract filename from addr")
	}
	return m.filename(fn), nil
}

func (m *Magazine) filename(fn string) string {
	if fe := path.Ext(fn); len(fe) == 0 {
		return fmt.Sprintf("%v.%v", fn, ext)
	}
	return fn
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
	totErrs      *int32
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
		totErrs:      new(int32),
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
	d.downloadMagazines(ctx, links)
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

		text := nd.Find("p.card-text").Text()
		text = strings.TrimSpace(text)
		if len(text) > 0 {
			altTxt = text
		}

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

func (d *dandyDownloader) downloadMagazines(ctx context.Context, magazines <-chan *Magazine) {
	for magazine := range magazines {
		select {
		case <-ctx.Done():
			return
		default:
			started := time.Now()
			err := d.downloadMagazine(ctx, magazine)
			if err != nil {
				magazine.Err = err
				d.incErrors()
			} else {
				d.incMagProcessed()
			}
			d.reportMagazine(magazine, time.Since(started))
		}
	}
}

func (d *dandyDownloader) downloadMagazine(ctx context.Context, m *Magazine) error {
	if m.Err != nil {
		return m.Err
	}

	fp, err := d.buildAndCheckMagazineFilepath(m)
	if err != nil {
		return err
	}

	rq, err := http.NewRequestWithContext(ctx, http.MethodGet, m.Addr, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(rq)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status code %v", resp.StatusCode)
	}

	out, err := os.Create(fp)
	if err != nil {
		return err
	}
	defer func() { _ = out.Close() }()

	size, err := io.Copy(out, resp.Body)
	if err != nil {
		if ok, _ := pathExists(fp); ok {
			_ = deleteFile(fp)
		}
		return err
	}
	m.Filepath = fp
	m.Size = size
	return nil
}

func (d *dandyDownloader) buildAndCheckMagazineFilepath(m *Magazine) (string, error) {
	fn, err := m.Filename()
	if err != nil {
		return "", err
	}

	fp := filepath.Join(d.output, m.Year.String(), fn)
	dir, _ := path.Split(fp)
	if ok, err := pathExists(dir); err != nil {
		return "", err
	} else if !ok {
		err = createDir(dir)
		if err != nil {
			return "", err
		}
		return fn, nil
	}

	if ok, err := pathExists(fp); err != nil {
		return "", err
	} else if ok {
		return "", fmt.Errorf("file %v already exists", fp)
	}
	return fp, nil
}

func (d *dandyDownloader) reportMagazine(m *Magazine, dur time.Duration) {
	d.reportOpt(func() interface{} {
		var sb strings.Builder
		sb.WriteString(fmt.Sprintf("processed for %v %v\n", dur, m))
		sb.WriteString(fmt.Sprintf("progress %v\n\n", d.stat()))
		return sb.String()
	})
}

func (d *dandyDownloader) reportStarted() {
	d.started = time.Now()

	var sb strings.Builder
	sb.WriteString("started downloading for ")
	if d.count == 1 {
		sb.WriteString(fmt.Sprintf("%v year", d.from))
	} else {
		sb.WriteString(fmt.Sprintf("%v years from %v to %v", d.count, d.from, d.to))
	}
	sb.WriteString("\n")
	d.report(sb.String())
}

func (d *dandyDownloader) reportFinished(fatalErr interface{}) {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("finished working for %v \n", time.Since(d.started)))
	sb.WriteString(fmt.Sprintf("statistic: %v \n", d.stat()))
	if fatalErr != nil {
		sb.WriteString(fmt.Sprintf("with fatal err %v", fatalErr))
	}
	d.report(sb.String())
}

func (d *dandyDownloader) report(data interface{}) {
	d.reportOpt(func() interface{} { return data })
}

func (d *dandyDownloader) reportOpt(data func() interface{}) {
	if !d.verbose {
		return
	}
	fmt.Printf("%v: %v", time.Now().Format("03:04:05.000"), data())
}

func (d *dandyDownloader) stat() string {
	return fmt.Sprintf("total years: %v processed: %v total magazines %v processed %v errors %v", *d.totYears, *d.totYearsDone, *d.totMags, *d.totMagsDone, *d.totErrs)
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

func (d *dandyDownloader) incErrors() {
	atomic.AddInt32(d.totErrs, 1)
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

func deleteFile(path string) error {
	return os.Remove(path)
}
