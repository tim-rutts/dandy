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
	"sync"
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
	sb.WriteString(fmt.Sprintf("Year %v Name %q Size %v Filepath %v\n", m.Year, m.Name, m.FormattedSize(), m.Filepath))
	sb.WriteString(fmt.Sprintf("Addr %q", m.Addr))
	if m.Err != nil {
		sb.WriteString(fmt.Sprintf("\nError %v", m.Err))
	}
	return sb.String()
}

func (m *Magazine) FormattedSize() string {
	const unit = 1000
	if m.Size < unit {
		return fmt.Sprintf("%d b", m.Size)
	}
	div, exp := int64(unit), 0
	for n := m.Size / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cb", float64(m.Size)/float64(div), "kMGTPE"[exp])
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
	Err() error
}

type dandyDownloader struct {
	from, to     int
	verbose      bool
	done         chan struct{}
	output       string
	startMx      sync.Mutex
	started      bool
	startedAt    time.Time
	totYears     int
	totYearsDone *int32
	totMags      *int32
	totMagsOk    *int32
	totMagsErrs  *int32
	fatalErr     error
}

func newDandyDownloader(from, to int, verbose bool, output string) *dandyDownloader {
	return &dandyDownloader{
		from:         from,
		to:           to,
		verbose:      verbose,
		output:       output,
		totYears:     to - from + 1,
		totYearsDone: new(int32),
		totMags:      new(int32),
		totMagsOk:    new(int32),
		totMagsErrs:  new(int32),
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

func (d *dandyDownloader) Err() error {
	return d.fatalErr
}

func (d *dandyDownloader) Run(ctx context.Context) <-chan struct{} {
	if d.started {
		return d.done
	}
	d.startMx.Lock()
	defer d.startMx.Unlock()

	if d.started {
		return d.done
	}

	d.done = make(chan struct{})
	go func() {
		defer func() {
			fatal := recover()
			d.stop(fatal)
		}()
		d.start(ctx)
	}()
	return d.done
}

func (d *dandyDownloader) stop(err interface{}) {
	if fatalErr, ok := err.(error); ok {
		d.fatalErr = fatalErr
	}

	d.reportFinished(err)
	close(d.done)
}

func (d *dandyDownloader) start(ctx context.Context) {
	d.startedAt = time.Now()
	d.started = true

	d.reportStarted()

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
					d.incMagTotal()
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
				d.incMagError()
			} else {
				d.incMagOk()
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
		return fp, nil
	}

	if ok, err := pathExists(fp); err != nil {
		return "", err
	} else if ok {
		return "", fmt.Errorf("file %v already exists", fp)
	}
	return fp, nil
}

func (d *dandyDownloader) reportMagazine(m *Magazine, dur time.Duration) {
	d.report(func() interface{} {
		var sb strings.Builder
		sb.WriteString(fmt.Sprintf("magazine %v\n", m))
		sb.WriteString(fmt.Sprintf("elapsed %v total %v\n", formatDur(dur), d.elapsedStr()))
		sb.WriteString(fmt.Sprintf("progress %v\n\n", d.stat()))
		return sb.String()
	})
}

func (d *dandyDownloader) reportStarted() {

	d.report(func() interface{} {
		var sb strings.Builder
		sb.WriteString("started downloading for ")
		if d.totYears == 1 {
			sb.WriteString(fmt.Sprintf("%v year", d.from))
		} else {
			sb.WriteString(fmt.Sprintf("%v years from %v to %v", d.totYears, d.from, d.to))
		}
		sb.WriteString("\n")
		return sb.String()
	})
}

func (d *dandyDownloader) reportFinished(fatalErr interface{}) {
	d.report(func() interface{} {
		var sb strings.Builder
		sb.WriteString(fmt.Sprintf("finished working for %v \n", d.elapsedStr()))
		sb.WriteString(fmt.Sprintf("statistic: %v \n", d.stat()))
		if fatalErr != nil {
			sb.WriteString(fmt.Sprintf("with fatal err %v", fatalErr))
		}
		return sb.String()
	})
}

func (d *dandyDownloader) report(data func() interface{}) {
	if !d.verbose {
		return
	}
	fmt.Printf("%v: %v", formatTime(time.Now()), data())
}

func (d *dandyDownloader) stat() string {
	return fmt.Sprintf("total years: %v processed: %v total magazines %v processed %v errors %v", d.totYears, *d.totYearsDone, *d.totMags, *d.totMagsOk, *d.totMagsErrs)
}

func (d *dandyDownloader) elapsed() time.Duration {
	return time.Since(d.startedAt)
}

func (d *dandyDownloader) elapsedStr() string {
	return formatDur(d.elapsed())
}

func (d *dandyDownloader) incYearProcessed() {
	atomic.AddInt32(d.totYearsDone, 1)
}

func (d *dandyDownloader) incMagTotal() {
	atomic.AddInt32(d.totMags, 1)
}

func (d *dandyDownloader) incMagOk() {
	atomic.AddInt32(d.totMagsOk, 1)
}

func (d *dandyDownloader) incMagError() {
	atomic.AddInt32(d.totMagsErrs, 1)
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

func formatTime(t time.Time) string {
	return t.Format("03:04:05.000")
}

func formatDur(dur time.Duration) string {
	rd := time.Millisecond
	s := dur.Seconds()
	if s > time.Minute.Seconds() {
		rd = time.Second
	}
	return fmt.Sprintf("%v", dur.Round(rd))
}
