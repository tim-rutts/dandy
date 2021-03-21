package main

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
)

const (
	testYearPageFilepath    = "testdata/1922.html"
	testYearPageExpectedNum = 28
)

type ytc struct {
	name string
	from int
	to   int
	ln   int
}

func newYTC(n string, f, t, l int) *ytc {
	return &ytc{
		name: n,
		from: f,
		to:   t,
		ln:   l,
	}
}

func (tc *ytc) String() string {
	return fmt.Sprintf("case %q from %v to %v ln %v", tc.name, tc.from, tc.to, tc.ln)
}

func TestInvalidYear(t *testing.T) {
	cases := []*ytc{
		newYTC("from lt min", MinYear-1, MaxYear, 0),
		newYTC("to gt max", MinYear, MaxYear+1, 0),
		newYTC("to min from", MaxYear, MinYear, 0),
	}

	for _, ytc := range cases {
		_, _, err := calcYearsRange(ytc.from, ytc.to, ytc.ln)
		if err == nil {
			t.Fatalf("%v (min %v max %v)", ytc, MinYear, MaxYear)
		}
	}
}

func TestYearLinkRange(t *testing.T) {
	cases := []*ytc{
		newYTC("from and to equal min", MinYear, MinYear, 1),
		newYTC("from and to equal max", MaxYear, MaxYear, 1),
		newYTC("one year", MinYear+1, MinYear+1, 1),
		newYTC("two years", MinYear+1, MinYear+2, 2),
		newYTC("ten years", MaxYear-9, MaxYear, 10),
	}

	countLinks := func(cl <-chan Year) int {
		var n int
		for range cl {
			n++
		}
		return n
	}

	for _, ytc := range cases {
		links := newDandyDownloader(ytc.from, ytc.to, false).genYears(context.Background())
		count := countLinks(links)
		if count != ytc.ln {
			t.Fatalf("%v actual ln %v", ytc, count)
		}
	}
}

func TestYearLinkFormat(t *testing.T) {
	c := newDandyDownloader(MinYear, MinYear, false).genYears(context.Background())
	y := <-c
	u := y.Addr()
	inxLP := strings.Index(u, yearPH)
	inxY := strings.Index(u, strconv.Itoa(MinYear))
	if inxLP != -1 || inxY == -1 {
		t.Fatalf("invalid year link format in %q", u)
	}
}

func TestYearsRange(t *testing.T) {
	type tc struct {
		in      *ytc
		expFrom int
		expTo   int
	}

	cases := []*tc{
		{newYTC("only from", MinYear, MinYear, 0), MinYear, MinYear},
		{newYTC("from and to", MinYear, MinYear+1, 0), MinYear, MinYear + 1},
		{newYTC("from and count", MinYear, 0, 2), MinYear, MinYear + 1},
		{newYTC("from and to and count", MinYear, MinYear+2, 10), MinYear, MinYear + 2},
	}

	for _, ytc := range cases {
		from, to, err := calcYearsRange(ytc.in.from, ytc.in.to, ytc.in.ln)
		if err != nil {
			t.Fatalf("%v err %v (min %v max %v)", ytc, MinYear, MaxYear, err)
		}
		if from != ytc.expFrom || to != ytc.expTo {
			t.Fatalf("%v act_from %v act_to %v", ytc, from, to)
		}
	}
}

func TestParseYearPage(t *testing.T) {
	f, err := os.Open(testYearPageFilepath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()

	dd := newDandyDownloader(MinYear, MinYear, false)
	mgs := dd.parseMagazines(context.Background(), f, Year(MinYear))

	var count int
	for mg := range mgs {
		if mg.Err != nil {
			t.Fatalf("err %v mg %q", mg.Err, mg)
		}
		if _, err = url.ParseRequestURI(mg.Addr); err != nil {
			t.Fatalf("err %v mg %q", err, mg)
		}
		count++
	}

	if testYearPageExpectedNum != count {
		t.Fatalf("expected %v actual %v", testYearPageExpectedNum, count)
	}
}
