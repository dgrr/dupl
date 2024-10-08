package main

import (
	"bufio"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/mibk/dupl/job"
	"github.com/mibk/dupl/printer"
	"github.com/mibk/dupl/suffixtree"
	"github.com/mibk/dupl/syntax"
)

const defaultThreshold = 15

var (
	paths         = []string{"."}
	vendor        = flag.Bool("vendor", false, "")
	verbose       = flag.Bool("verbose", false, "")
	fromThreshold = flag.Int("from-threshold", defaultThreshold, "")
	toThreshold   = flag.Int("to-threshold", defaultThreshold, "")
	files         = flag.Bool("files", false, "")

	html     = flag.Bool("html", false, "")
	plumbing = flag.Bool("plumbing", false, "")
)

const (
	vendorDirPrefix = "vendor" + string(filepath.Separator)
	vendorDirInPath = string(filepath.Separator) + vendorDirPrefix
)

func init() {
	flag.BoolVar(verbose, "v", false, "alias for -verbose")
	flag.IntVar(fromThreshold, "t", defaultThreshold, "alias for -threshold")
}

func main() {
	flag.Usage = usage
	flag.Parse()
	if *html && *plumbing {
		log.Fatal("you can have either plumbing or HTML output")
	}
	if flag.NArg() > 0 {
		paths = flag.Args()
	}

	if *verbose {
		log.Println("Building suffix tree")
	}
	schan := job.Parse(filesFeed())
	t, data, done := job.BuildTree(schan)
	<-done

	// finish stream
	t.Update(&syntax.Node{Type: -1})

	if *verbose {
		log.Println("Searching for clones")
	}

	newPrinter := printer.NewText
	if *html {
		newPrinter = printer.NewHTML
	} else if *plumbing {
		newPrinter = printer.NewPlumbing
	}
	p := newPrinter(os.Stdout, ioutil.ReadFile)

	duplChans := make([]<-chan syntax.Match, 0)
	for i := *fromThreshold; i >= *toThreshold; i -= 1 {
		mchan := t.FindDuplOver(i)
		duplChan := make(chan syntax.Match)
		go findDuplicates(data, i, mchan, duplChan)
		duplChans = append(duplChans, duplChan)
	}

	if err := printDupls(p, duplChans); err != nil {
		log.Fatal(err)
	}
}

func findDuplicates(data *[]*syntax.Node, threshold int, mchan <-chan suffixtree.Match, duplChan chan<- syntax.Match) {
	for m := range mchan {
		match := syntax.FindSyntaxUnits(*data, m, threshold)
		if len(match.Frags) > 0 {
			// this match should contain all the filenames to avoid duplicates within the same file
			// and just print out the same file.
			matchesFiles := func() bool {
				// just use a map, it's easy to compare
				pathMap := make(map[string]struct{})
				for _, path := range paths {
					pathMap[path] = struct{}{}
				}

				for i := 0; i < len(match.Frags) && len(pathMap) != 0; i++ {
					for _, node := range match.Frags[i] {
						for parentPath, _ := range pathMap {
							if strings.HasPrefix(node.Filename, parentPath) {
								delete(pathMap, parentPath)
								break
							}
						}
					}
				}

				return len(pathMap) == 0
			}

			if matchesFiles() {
				duplChan <- match
			}
		}
	}
	close(duplChan)
}

func filesFeed() chan string {
	if *files {
		fchan := make(chan string)
		go func() {
			s := bufio.NewScanner(os.Stdin)
			for s.Scan() {
				f := s.Text()
				fchan <- strings.TrimPrefix(f, "./")
			}
			close(fchan)
		}()
		return fchan
	}
	return crawlPaths(paths)
}

func crawlPaths(paths []string) chan string {
	fchan := make(chan string)
	go func() {
		for _, path := range paths {
			info, err := os.Lstat(path)
			if err != nil {
				log.Fatal(err)
			}
			if !info.IsDir() {
				fchan <- path
				continue
			}
			err = filepath.Walk(path, func(path string, info os.FileInfo, err error) error {
				if !*vendor && (strings.HasPrefix(path, vendorDirPrefix) ||
					strings.Contains(path, vendorDirInPath)) {
					return nil
				}
				if !info.IsDir() && strings.HasSuffix(info.Name(), ".go") {
					fchan <- path
				}
				return nil
			})
			if err != nil {
				log.Fatal(err)
			}
		}
		close(fchan)
	}()
	return fchan
}

func printDupls(p printer.Printer, duplChans []<-chan syntax.Match) error {
	groups := make(map[string][][]*syntax.Node)
	for _, duplChan := range duplChans {
		for dupl := range duplChan {
			groups[dupl.Hash] = append(groups[dupl.Hash], dupl.Frags...)
		}
	}
	keys := make([]string, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	if err := p.PrintHeader(); err != nil {
		return err
	}
	for _, k := range keys {
		uniq := unique(groups[k])
		if len(uniq) > 1 {
			if err := p.PrintClones(uniq); err != nil {
				return err
			}
		}
	}
	return p.PrintFooter()
}

func unique(group [][]*syntax.Node) [][]*syntax.Node {
	fileMap := make(map[string]map[int]struct{})

	var newGroup [][]*syntax.Node
	for _, seq := range group {
		node := seq[0]
		file, ok := fileMap[node.Filename]
		if !ok {
			file = make(map[int]struct{})
			fileMap[node.Filename] = file
		}
		if _, ok := file[node.Pos]; !ok {
			file[node.Pos] = struct{}{}
			newGroup = append(newGroup, seq)
		}
	}
	return newGroup
}

func usage() {
	fmt.Fprintln(os.Stderr, `Usage: dupl [flags] [paths]

Paths:
  If the given path is a file, dupl will use it regardless of
  the file extension. If it is a directory, it will recursively
  search for *.go files in that directory.

  If no path is given, dupl will recursively search for *.go
  files in the current directory.

Flags:
  -files
    	read file names from stdin one at each line
  -html
    	output the results as HTML, including duplicate code fragments
  -plumbing
    	plumbing (easy-to-parse) output for consumption by scripts or tools
  -from-threshold size
    	minimum token sequence size as a clone (default 15)
  -to-threshold size
        maximum token sequence size as a clone (default 15)
  -vendor
    	check files in vendor directory
  -v, -verbose
    	explain what is being done

Examples:
  dupl -t 100
    	Search clones in the current directory of size at least
    	100 tokens.
  dupl $(find app/ -name '*_test.go')
    	Search for clones in tests in the app directory.
  find app/ -name '*_test.go' |dupl -files
    	The same as above.`)
	os.Exit(2)
}
