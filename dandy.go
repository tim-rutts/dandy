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
	"runtime/debug"
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
	sb.WriteString(fmt.Sprintf("Magazine %q for %v year Size %q Filepath %q\n", m.Name, m.Year, formatFileSize(m.Size), m.Filepath))
	sb.WriteString(fmt.Sprintf("Addr %q", m.Addr))
	if m.Err != nil {
		sb.WriteString(fmt.Sprintf("\nError %v", m.Err))
	}
	return sb.String()
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

type FatalError struct {
	stack []byte
	err   error
}

func (e *FatalError) Error() string {
	return e.err.Error()
}

func (e *FatalError) ErrorStack() string {
	if len(e.stack) == 0 {
		return "no error stack"
	}
	return string(e.stack)
}

type fileExistError struct {
	err error
}

func (e *fileExistError) Error() string {
	return e.err.Error()
}

type MagazineExistsError struct {
	err error
	mag *Magazine
}

func (e *MagazineExistsError) Error() string {
	return fmt.Sprintf("magazine %q for %v year %v", e.mag.Name, e.mag.Year, e.err)
}

type MagazineError struct {
	err error
	mag *Magazine
}

func (e *MagazineError) Error() string {
	return fmt.Sprintf("magazine %q for %v year %v", e.mag.Name, e.mag.Year, e.err)
}

type Downloader interface {
	Run(ctx context.Context) <-chan struct{}
	Err() error
	Status() string
	YearFrom() int
	YearTo() int
	MagazineErrs() <-chan error
}

type dandyDownloader struct {
	from, to     int
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
	totSize      *int64
	fatalErr     error
	ctxCancel    context.CancelFunc
	stopped      bool
	stopMx       sync.Mutex
	magErrsChan  chan error
	magErrsSub   bool
	magErrsMx    sync.Mutex
}

func newDandyDownloader(from, to int, output string) *dandyDownloader {
	return &dandyDownloader{
		from:         from,
		to:           to,
		output:       output,
		totYears:     to - from + 1,
		totYearsDone: new(int32),
		totMags:      new(int32),
		totMagsOk:    new(int32),
		totMagsErrs:  new(int32),
		totSize:      new(int64),
	}
}

func NewDownloader(from, to, count int, output string) (Downloader, error) {
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

	return newDandyDownloader(f, t, output), nil
}

func (d *dandyDownloader) YearFrom() int {
	return d.from
}

func (d *dandyDownloader) YearTo() int {
	return d.to
}

func (d *dandyDownloader) Status() string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("elapsed: %v magazines: %v", d.elapsedStr(), *d.totMags))
	if d.totYears > 1 {
		sb.WriteString(fmt.Sprintf(" for %v(%v) years", *d.totYearsDone, d.totYears))
	}
	sb.WriteString(fmt.Sprintf(" ok: %v err: %v downloaded: %v", *d.totMagsOk, *d.totMagsErrs, formatFileSize(*d.totSize)))

	return sb.String()
}

func (d *dandyDownloader) Err() error {
	return d.fatalErr
}

func (d *dandyDownloader) MagazineErrs() <-chan error {
	if d.magErrsSub {
		return d.magErrsChan
	}
	d.magErrsMx.Lock()
	defer d.magErrsMx.Unlock()
	if d.magErrsSub {
		return d.magErrsChan
	}
	d.magErrsSub = true
	d.magErrsChan = make(chan error, 10)
	return d.magErrsChan
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
	go d.start(ctx)
	return d.done
}

func (d *dandyDownloader) stop(err error) {
	if d.fatalErr == nil && err != nil {
		d.fatalErr = err
	}

	if d.stopped {
		return
	}
	d.stopMx.Lock()
	defer d.stopMx.Unlock()
	if d.stopped {
		return
	}

	d.stopped = true
	d.ctxCancel()
	if d.magErrsSub {
		close(d.magErrsChan)
	}
	close(d.done)
}

func (d *dandyDownloader) start(ctx context.Context) {
	ctxWC, cancel := context.WithCancel(ctx)
	defer d.guard()

	d.startedAt = time.Now()
	d.started = true
	d.ctxCancel = cancel

	years := d.genYears(ctxWC)
	pages := d.downloadYearPages(ctxWC, years)
	links := d.parseYearPages(ctxWC, pages)
	d.downloadMagazines(ctxWC, links)
	d.stop(nil)
}

func (d *dandyDownloader) guard() {
	if fe := recover(); fe != nil {
		err := &FatalError{}
		err.err = fmt.Errorf("%v", fe)
		err.stack = debug.Stack()
		d.stop(err)
	}
}

func (d *dandyDownloader) downloadYearPages(ctx context.Context, years <-chan Year) <-chan *YearPage {
	c := make(chan *YearPage)
	go func() {
		defer close(c)
		defer d.guard()
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
		defer d.guard()
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
		defer d.guard()
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
			err := d.downloadMagazine(ctx, magazine)
			if err != nil {
				d.incMagError()
				d.reportError(ctx, magazine, err)
			} else {
				d.incMagOk()
				d.incSize(magazine.Size)
			}
		}
	}
}

func (d *dandyDownloader) downloadMagazine(ctx context.Context, m *Magazine) error {
	if m.Err != nil {
		return m.Err
	}

	fp, err := buildAndCheckMagFilepath(d.output, m)
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

func (d *dandyDownloader) reportError(ctx context.Context, m *Magazine, err error) {
	if !d.magErrsSub {
		return
	}

	te := typedMagErr(m, err)
	select {
	case d.magErrsChan <- te:
	case <-ctx.Done():
		return
	}
}

func (d *dandyDownloader) elapsed() time.Duration {
	return time.Since(d.startedAt)
}

func (d *dandyDownloader) elapsedStr() string {
	return formatDur(d.elapsed())
}

func (d *dandyDownloader) incSize(size int64) {
	if size == 0 {
		return
	}
	atomic.AddInt64(d.totSize, size)
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

func typedMagErr(m *Magazine, err error) error {
	if _, ok := err.(*fileExistError); ok {
		return &MagazineExistsError{err: err, mag: m}
	}
	return &MagazineError{err: err, mag: m}
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

func formatDur(dur time.Duration) string {
	rd := time.Millisecond
	s := dur.Seconds()
	if s > time.Minute.Seconds() {
		rd = time.Second
	}
	return fmt.Sprintf("%v", dur.Round(rd))
}

func buildAndCheckMagFilepath(output string, m *Magazine) (string, error) {
	fp, err := buildMagFilepath(output, m.Name, m.Addr, m.Year)
	if err != nil {
		return "", err
	}

	dir, fn := path.Split(fp)
	m.Name = fn
	m.Filepath = fp

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
		return "", &fileExistError{err: fmt.Errorf("file %q already exists", fp)}
	}
	return fp, nil
}

func buildMagFilepath(output, name, addr string, year Year) (string, error) {
	fp := func(fn string) string {
		return filepath.Join(output, year.String(), fn)
	}

	if len(name) > 0 {
		return fp(addFileExt(name)), nil
	}

	if len(addr) == 0 {
		return "", errors.New("addr is empty")
	}

	_, fn := path.Split(addr)
	if len(fn) == 0 {
		return "", errors.New("cannot extract filename from addr")
	}
	return fp(addFileExt(fn)), nil
}

func addFileExt(fn string) string {
	if fe := path.Ext(fn); len(fe) == 0 {
		return fmt.Sprintf("%v.%v", fn, ext)
	}
	return fn
}

func formatFileSize(size int64) string {
	const unit = 1000
	if size < unit {
		return fmt.Sprintf("%d b", size)
	}
	div, exp := int64(unit), 0
	for n := size / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cb", float64(size)/float64(div), "kMGTPE"[exp])
}
