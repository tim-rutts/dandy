package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"

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

func newErrMagazine(y Year, err error) *Magazine {
	return &Magazine{
		Year: y,
		Addr: "Unknown",
		Name: "Unknown",
		Err:  err,
	}
}

type Year int

func (y *Year) String() string {
	return strconv.Itoa(int(*y))
}

func (y *Year) Addr() string {
	return strings.ReplaceAll(yearUrl, yearPH, y.String())
}

type Downloader interface {
	Run(ctx context.Context) <-chan struct{}
}

type dandyDownloader struct {
	from, to, count int
	verbose         bool
	done            chan struct{}
}

func newDandyDownloader(from, to int, verbose bool) *dandyDownloader {
	return &dandyDownloader{
		from:    from,
		to:      to,
		count:   from - to + 1,
		verbose: verbose,
	}
}

func NewDownloader(from, to, count int, verbose bool) (Downloader, error) {
	f, t, err := calcYearsRange(from, to, count)
	if err != nil {
		return nil, err
	}

	return newDandyDownloader(f, t, verbose), nil
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
	panic("not implemented")
}

func (d *dandyDownloader) parseMagazines(ctx context.Context, r io.Reader, y Year) <-chan *Magazine {
	c := make(chan *Magazine)
	go func() {
		defer close(c)
		push := func(m *Magazine) {
			select {
			case c <- m:
				return
			case <-ctx.Done():
				return
			}
		}
		doc, err := goquery.NewDocumentFromReader(r)
		if err != nil {
			push(newErrMagazine(y, err))
			return
		}

		set := doc.Find("div.card a")
		if set == nil || len(set.Nodes) == 0 {
			push(newErrMagazine(y, errors.New("no links found")))
			return
		}

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

			magazine := &Magazine{
				Year: y,
				Addr: link,
				Name: altTxt,
			}
			push(magazine)
		}
	}()
	return c
}

func (d *dandyDownloader) genYears(ctx context.Context) <-chan Year {
	c := make(chan Year)
	go func() {
		defer close(c)
		for i := d.from; i <= d.to; i++ {
			select {
			case c <- Year(i):
				break
			case <-ctx.Done():
				return
			}
		}
	}()
	return c
}

func (d *dandyDownloader) reportStarted() {
	d.report(fmt.Sprintf("started working for years from %v to %v (total year(s) %v)", d.from, d.to, d.count))
}

func (d *dandyDownloader) reportFinished(fatalErr interface{}) {
	if fatalErr != nil {
		d.report(fmt.Sprintf("finished working with fatal err %v", fatalErr))
		return
	}
	d.report(fmt.Sprintf("finished working"))
}

func (d *dandyDownloader) report(data interface{}) {
	if !d.verbose {
		return
	}
	fmt.Println(data)
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
