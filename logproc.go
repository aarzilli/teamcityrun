package main

import (
	"io"
	"net/http"
	"fmt"
	"strconv"
	"bufio"
	"strings"
	"encoding/json"
)

func downloadLog(buildId int) io.Reader {
	req, err := http.NewRequest("GET", fmt.Sprintf("https://%s/downloadBuildLog.html?buildId=%d", TEAMCITY_HOST, buildId), nil)
	req.Header.Add("Authorization", fmt.Sprintf("Bearer %s", TEAMCITY_TOKEN))
	req.Header.Add("Accept", "text/text")
	resp, err := http.DefaultClient.Do(req)
	must(err)
	return resp.Body
}

type logline struct {
	raw       string
	time      int
	indent    int
	tags      []string
	text      string
	addtext   string
	testEvent *testEvent
}

type testEvent struct {
	Time    string
	Action  string
	Package string
	Test    string
	Elapsed float64 // seconds
	Output  string
}

func logparse(line string) *logline {
	rest := line

	perr := func(reason string) {
		panic(fmt.Errorf("could not parse %q: %s", line, reason))
	}

	expectByte := func(b byte) {
		if len(rest) == 0 || rest[0] != b {
			perr(fmt.Sprintf("expecting %c", b))
		}
		rest = rest[1:]
	}

	expectLen := func(n int) string {
		if len(rest) < n {
			perr(fmt.Sprintf("expecting %d characters", n))
		}
		r := rest[:n]
		rest = rest[n:]
		return r
	}

	consumeMaybe := func(b byte) {
		if len(rest) > 0 && rest[0] == b {
			rest = rest[1:]
		}
	}

	if len(line) > 0 && line[0] != '[' {
		return nil
	}

	var ll logline

	ll.raw = line

	// timestamp
	expectByte('[')
	hour, _ := strconv.Atoi(expectLen(2))
	expectByte(':')
	minute, _ := strconv.Atoi(expectLen(2))
	expectByte(':')
	second, _ := strconv.Atoi(expectLen(2))
	expectByte(']')

	ll.time = hour*60*60 + minute*60 + second

	expectLen(1) // flags?
	expectByte(':')

	// indentation
	for len(rest) > 0 && rest[0] == '\t' {
		ll.indent++
		rest = rest[1:]
	}

	// tags
	for {
		consumeMaybe(' ')
		if len(rest) <= 0 || rest[0] != '[' {
			break
		}

		rest = rest[1:]
		found := false
		for i := 0; i < len(rest); i++ {
			if rest[i] == ']' {
				ll.tags = append(ll.tags, rest[:i])
				rest = rest[i+1:]
				found = true
				break
			}
		}
		if !found {
			break
		}
	}

	ll.text = rest

	return &ll
}

func cleanupLog(logbody io.Reader, verbose int) {
	const (
		verboseNothing       = iota
		verboseGoTestVerbose // approximately equivalent to 'go test -v'
		verboseTestOutput    // remove most of TeamCity output
		verboseAllText
	)

	var mode uint16

	const (
		modeRawText                = 1 << iota // shows the raw text of the output, no processing
		modeShowHeader                         // show the TeamCity header
		modeShowTestOutput                     // show entries marked with the [Test Output] tag
		modeShowRoot                           // show entries without any tags
		modeShowStep1                          // show entries marked with the [Step 1/2] tag (anywhere)
		modeShowStep2                          // show entries marked with the [Step 2/2] tag (anywhere)
		modeShowStep2Top                       // show entries marked with the [Step 2/2] tag (only if it's the topmost tag)
		modeShowStep2OutputActions             // show all output actions in step2
		modeSkipBeforeDwz                      // skip Step 2/2 messages that happen before the dwz message
		modeSkipJson                           // do not print things that are recognizable as JSON output of 'go test'
		modeSkipBeforeMakeTest                 // skip Step 2/2 messages that happen before the make test message
		modeMassaged                           // show massaged format for modeShowStep1, modeShowStep2, modeShowHeader and modeShowTestOutput
		modeShowOnlyFailed
	)

	switch verbose {
	case verboseNothing:
		mode = modeShowHeader | modeShowStep2Top | modeMassaged | modeSkipBeforeMakeTest | modeShowOnlyFailed | modeSkipJson
	case verboseGoTestVerbose:
		mode = modeShowHeader | modeShowStep2Top | modeShowStep2OutputActions | modeMassaged | modeSkipJson | modeSkipBeforeDwz
	case verboseTestOutput:
		mode = modeShowHeader | modeShowRoot | modeShowStep1 | modeShowStep2 | modeShowTestOutput | modeMassaged | modeSkipJson
	default:
		fallthrough
	case verboseAllText:
		mode = modeRawText | modeShowHeader
	}

	s := bufio.NewScanner(logbody)

	// build header
	for s.Scan() {
		if mode&modeShowHeader != 0 {
			fmt.Printf("%s\n", s.Text())
		}
		if s.Text() == "" {
			break
		}
	}

	stack := make([]string, 0, 20)

	treeize := func(ll *logline) {
		pl := len(stack)
		stack = stack[:ll.indent]
		for i := pl; i < len(stack); i++ {
			stack[i] = ""
		}

		for i := range ll.tags {
			stack[len(stack)-len(ll.tags)+i] = ll.tags[i]
		}
	}

	topOfStackIs := func(s string) bool {
		return len(stack) > 0 && stack[len(stack)-1] == s
	}

	stackHas := func(s string) bool {
		for _, z := range stack {
			if z == s {
				return true
			}
		}
		return false
	}

	var lastTime int
	first := true
	firstMassaged := true
	afterDwz := false
	afterMakeTest := false

	cached := []*logline{}

	for s.Scan() {
		if mode&modeRawText != 0 {
			fmt.Printf("%s\n", s.Text())
			continue
		}

		if strings.HasSuffix(s.Text(), " tests processed.") {
			if mode&modeShowHeader != 0 {
				fmt.Printf("%s\n", s.Text())
			}
			break
		}

		if strings.HasPrefix(s.Text(), "Current time: ") {
			fmt.Printf("%s\n", s.Text())
			break
		}

		ll := logparse(s.Text())
		if ll == nil {
			// weird unparsable line?
			continue
		}
		treeize(ll)
		if topOfStackIs("Test Output") {
			if !s.Scan() {
				panic(fmt.Errorf("test output not followed by a line"))
			}
			ll.addtext = s.Text()
		}

		buildStep := topOfStackIs("Step 2/2") || topOfStackIs("Step 1/1")

		if buildStep {
			if len(ll.text) > 0 && ll.text[0] == '{' {
				te := &testEvent{}
				err := json.Unmarshal([]byte(ll.text), te)
				if err == nil {
					if te.Action != "" {
						ll.testEvent = te
					}
				}
			}
		}

		if !afterDwz {
			if buildStep {
				if strings.HasPrefix(ll.text, "+ dwz --version") {
					afterDwz = true
				}
			}
		}

		if !afterMakeTest {
			if buildStep {
				if strings.HasPrefix(ll.text, "+ make test") {
					afterMakeTest = true
				}
			}
		}

		if !afterDwz || !afterMakeTest {
			if buildStep {
				if strings.HasPrefix(ll.text, "Finding latest patch") {
					afterMakeTest = true
					afterDwz = true
				}
			}
		}

		if first {
			first = false
			lastTime = ll.time
		}

		emitted := false

		emitMassaged := func(text string) {
			if firstMassaged {
				firstMassaged = false
				fmt.Printf("  ΔT\tTEXT\n")
			}
			if len(text) > 0 && text[len(text)-1] == '\n' {
				text = text[:len(text)-1]
				if len(text) > 0 && text[len(text)-1] == '\r' {
					text = text[:len(text)-1]
				}
			}
			if ll.time-lastTime > 0 {
				fmt.Printf("% 4d\t%s\n", ll.time-lastTime, text)
			} else {
				fmt.Printf("    \t%s\n", text)
			}
			lastTime = ll.time
		}

		emitText := func() {
			if emitted {
				return
			}
			emitted = true
			emitMassaged(ll.text)
		}

		emitRaw := func() {
			if emitted {
				return
			}
			emitted = true
			fmt.Printf("%s\n", ll.raw)
		}

		if mode&modeShowRoot != 0 {
			if len(stack) == 0 {
				if mode&modeMassaged != 0 {
					emitText()
				} else {
					emitRaw()
				}
			}
		}

		if mode&modeShowStep1 != 0 {
			if stackHas("Step 1/2") {
				if mode&modeMassaged != 0 {
					emitText()
				} else {
					emitRaw()
				}
			}
		}

		if mode&modeShowStep2 != 0 || mode&modeShowStep2Top != 0 {
			if stackHas("Step 2/2") || stackHas("Step 1/1") {
				shouldShow := true
				if mode&modeSkipBeforeDwz != 0 && !afterDwz {
					shouldShow = false
				}
				if mode&modeSkipJson != 0 && ll.testEvent != nil {
					shouldShow = false
				}
				if mode&modeShowStep2Top != 0 && !topOfStackIs("Step 2/2") {
					shouldShow = false
				}
				if mode&modeSkipBeforeMakeTest != 0 && !afterMakeTest {
					shouldShow = false
				}
				if shouldShow {
					if mode&modeMassaged != 0 {
						if !topOfStackIs("Test Output") {
							emitText()
						}
					} else {
						if topOfStackIs("Test Output") {
							if mode&modeShowTestOutput != 0 {
								emitRaw()
							}
						} else {
							emitRaw()
						}
					}
				}
			}
		}

		if mode&modeShowTestOutput != 0 {
			if topOfStackIs("Test Output") {
				if mode&modeMassaged != 0 {
					emitMassaged(ll.addtext)
				} else {
					fmt.Printf("%s\n", ll.addtext)
				}
			}
		}

		if mode&modeShowOnlyFailed != 0 && buildStep && strings.HasPrefix(ll.text, "Go ") {
			emitText()
		}

		if mode&modeShowStep2OutputActions != 0 && ll.testEvent != nil {
			if ll.testEvent.Action == "output" {
				emitMassaged(ll.testEvent.Output)
			}
		}

		if mode&modeShowOnlyFailed == 0 && ll.testEvent != nil && ll.testEvent.Action == "fail" && mode&modeShowStep2OutputActions == 0 {
			emitMassaged(fmt.Sprintf("FAIL\t%s", ll.testEvent.Package))
		}

		if mode&modeShowOnlyFailed != 0 && ll.testEvent != nil {
			dumpCached := func() {
				for i := range cached {
					ll = cached[i] // needed by emitMassaged to know the test time
					switch ll.testEvent.Action {
					case "output":
						emitMassaged(ll.testEvent.Output)
					}
				}
				cached = cached[:0]
			}

			cached = append(cached, ll)
			if ll.testEvent.Test == "" {
				switch ll.testEvent.Action {
				case "pass":
					emitMassaged(fmt.Sprintf("%s\t%gs", ll.testEvent.Package, ll.testEvent.Elapsed))
					cached = cached[:0]
				case "skip":
					emitMassaged(fmt.Sprintf("%s\t[no test files]", ll.testEvent.Package))
					cached = cached[:0]
				case "output":
					// do nothing
				case "fail":
					savedll := ll
					dumpCached()
					ll = savedll
					emitMassaged(fmt.Sprintf("%s\tFAIL", ll.testEvent.Package))
					cached = cached[:0]
				default:
					emitMassaged(fmt.Sprintf("%s\t%s", ll.testEvent.Package, ll.testEvent.Action))
					cached = cached[:0]
				}
			} else {
				switch ll.testEvent.Action {
				case "pass":
					cached = cached[:0]
				case "fail":
					dumpCached()
				}
			}
		}
	}
}
