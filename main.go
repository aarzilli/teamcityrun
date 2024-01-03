package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
)

func usage() {
	fmt.Fprintf(os.Stderr, `Usage:
	
	teamcityrun <regex>		runs current diff on all build types matched (case insensitive) by regex
	teamcityrun buildtypes		lists all available build types
	teamcityrun status <build-id>	shows status of build
	teamcityrun status		shows status of the last 200 builds on the default branch
	teamcityrun summary	shows summary of the last 200 builds
	teamcityrun log <build-id> [-v] shows log for build, cleaned up, add more -v to clean up less
					can also specify a text file instead of a build-id
	teamcityrun diff		shows current diff

Environment variables TEAMCITY_TOKEN and TEAMCITY_HOST must be set.
	
`)
	os.Exit(1)
}

// Reference:
//  https://www.jetbrains.com/help/teamcity/cloud/2021.1/personal-build.html#Direct+Patch+Upload
//  https://www.jetbrains.com/help/teamcity/rest/teamcity-rest-api-documentation.html
//  https://www.jetbrains.com/help/teamcity/rest-api-reference.html

func must(err error) {
	if err != nil {
		panic(err)
	}
}

var TEAMCITY_TOKEN, TEAMCITY_HOST string

type hdopts struct {
	ContentType string
	Accept      string
}

func httpdo(method string, opts hdopts, path string, body io.Reader) *http.Response {
	req, err := http.NewRequest(method, "https://"+TEAMCITY_HOST+path, body)
	must(err)
	req.Header.Add("Authorization", fmt.Sprintf("Bearer %s", TEAMCITY_TOKEN))
	if opts.ContentType != "" {
		req.Header.Add("Content-Type", opts.ContentType)
	}
	if opts.Accept != "" {
		req.Header.Add("Accept", opts.Accept)
	}
	req.Header.Add("Origin", TEAMCITY_HOST)
	resp, err := http.DefaultClient.Do(req)
	must(err)
	return resp
}

func readall(body io.ReadCloser) []byte {
	buf, err := ioutil.ReadAll(body)
	must(body.Close())
	must(err)
	return buf
}

func uploadPatch(buildName string, diff []byte) string {
	resp := httpdo("POST", hdopts{ContentType: "text/text"}, fmt.Sprintf("/uploadDiffChanges.html?description=%s&commitType=0", buildName), bytes.NewReader(diff))
	return strings.TrimSpace(string(readall(resp.Body)))
}

func triggerBuild(buildTypeId, changeId string) {
	build := []byte(fmt.Sprintf(`<build personal="true">
  <triggered type='idePlugin' details='Unified Diff Patch'/>
  <triggeringOptions cleanSources="false" rebuildAllDependencies="false" queueAtTop="false"/>
  <buildType id="%s"/>
  <lastChanges>
    <change id="%s" personal="true"/>
  </lastChanges>
</build>`, buildTypeId, changeId))
	resp := httpdo("POST", hdopts{ContentType: "application/xml", Accept: "application/json"}, "/app/rest/buildQueue", bytes.NewReader(build))
	buf := readall(resp.Body)
	bs := decodeBuildStatus(bytes.NewReader(buf))
	fmt.Printf("%s\n", bs.URL())
}

func getdiff() []byte {
	exec.Command("git", "commit", "-a", "-m", "temp").CombinedOutput()
	cmd := exec.Command("git", "diff", "master")
	stdout, err := cmd.StdoutPipe()
	must(err)
	var buf []byte
	done := make(chan struct{})
	go func() {
		buf, _ = ioutil.ReadAll(stdout)
		close(done)
	}()
	cmd.Start()
	cmd.Wait()
	<-done
	return buf
}

type buildStatus struct {
	Id                int
	BuildTypeId       string
	State             string
	Status            string
	StatusText        string
	FinishOnAgentDate string
}

func decodeBuildStatus(rd io.Reader) *buildStatus {
	var bs buildStatus
	must(json.NewDecoder(rd).Decode(&bs))
	return &bs
}

func (bs *buildStatus) URL() string {
	return fmt.Sprintf("https://%s/viewLog.html?buildId=%d", TEAMCITY_HOST, bs.Id)
}

func getBuildStatus(buildId string) {
	resp := httpdo("GET", hdopts{ContentType: "application/json", Accept: "application/json"}, fmt.Sprintf("/app/rest/builds/id:%s", buildId), nil)
	buf := readall(resp.Body)
	bs := decodeBuildStatus(bytes.NewReader(buf))
	w := tabwriter.NewWriter(os.Stdout, 8, 8, 1, ' ', 0)
	defer w.Flush()
	fmt.Fprintf(w, "URL:\t%s\n", bs.URL())
	fmt.Fprintf(w, "Build Type:\t%s\n", bs.BuildTypeId)
	fmt.Fprintf(w, "State:\t%s\n", bs.State)
	fmt.Fprintf(w, "Status:\t%s\n", bs.Status)
	fmt.Fprintf(w, "Text:\t%s\n", bs.StatusText)
}

func getBuildTypes() []string {
	type buildType struct {
		Id string
	}

	type buildTypes struct {
		BuildType []buildType
	}

	resp := httpdo("GET", hdopts{Accept: "application/json"}, "/app/rest/buildTypes", nil)
	defer resp.Body.Close()
	var bts buildTypes
	must(json.NewDecoder(resp.Body).Decode(&bts))
	r := make([]string, len(bts.BuildType))
	for i := range bts.BuildType {
		r[i] = bts.BuildType[i].Id
	}
	return r
}

type buildStatusList struct {
	Build []buildStatus
}

func getBuildStatusAll() {
	resp := httpdo("GET", hdopts{ContentType: "application/json", Accept: "application/json"}, fmt.Sprintf("/app/rest/builds?locator=count:200"), nil)
	defer resp.Body.Close()
	var bslist buildStatusList
	must(json.NewDecoder(resp.Body).Decode(&bslist))
	w := tabwriter.NewWriter(os.Stdout, 8, 8, 1, ' ', 0)
	defer w.Flush()
	for _, build := range bslist.Build {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", build.BuildTypeId, build.State, build.Status, build.FinishOnAgentDate)
	}
}

func getBuildStatusSummary() {
	resp := httpdo("GET", hdopts{ContentType: "application/json", Accept: "application/json"}, fmt.Sprintf("/app/rest/builds?locator=count:200"), nil)
	defer resp.Body.Close()
	var bslist buildStatusList
	must(json.NewDecoder(resp.Body).Decode(&bslist))

	btypes := getBuildTypes()

	bslast := map[string]string{}
	bstot := map[string]int{}
	bssucc := map[string]int{}
	for _, build := range bslist.Build {
		if build.State != "finished" {
			continue
		}
		if bslast[build.BuildTypeId] == "" {
			bslast[build.BuildTypeId] = build.Status
		}
		bstot[build.BuildTypeId]++
		if build.Status == "SUCCESS" {
			bssucc[build.BuildTypeId]++
		}
	}

	conv := func(s string) string {
		switch s {
		case "FAILURE":
			return "FAIL"
		case "SUCCESS":
			return "OK"
		default:
			return s
		}
	}

	w := tabwriter.NewWriter(os.Stdout, 8, 8, 1, ' ', 0)
	plats := map[string]struct{}{}
	vers := map[string]struct{}{}
	some := false
	for _, btype := range btypes {
		v := strings.SplitN(btype, "_", 4)
		if len(v) != 4 || !strings.HasPrefix(btype, "Delve_") {
			fmt.Fprintf(w, "%s\t%s\t%d/%d\n", btype, conv(bslast[btype]), bssucc[btype], bstot[btype])
			some = true
			continue
		}
		plats[v[1]+"/"+v[2]] = struct{}{}
		vers[v[3]] = struct{}{}
	}
	w.Flush()

	platstrs := []string{}
	for k := range plats {
		platstrs = append(platstrs, k)
	}
	sort.Strings(platstrs)

	verstrs := []string{}
	for k := range vers {
		verstrs = append(verstrs, k)
	}
	sort.Strings(verstrs)

	if some {
		fmt.Println()
	}

	w = tabwriter.NewWriter(os.Stdout, 8, 8, 5, ' ', 0)
	fmt.Fprintf(w, "\t%s\n", strings.Join(verstrs, "\t"))

	for _, plat := range platstrs {
		fmt.Fprintf(w, "%s", plat)
		for _, ver := range verstrs {
			btype := fmt.Sprintf("Delve_%s_%s", strings.Replace(plat, "/", "_", -1), ver)
			if bslast[btype] != "" {
				fmt.Fprintf(w, "\t%s %d/%d", conv(bslast[btype]), bssucc[btype], bstot[btype])
			} else {
				fmt.Fprintf(w, "\t")
			}
		}
		fmt.Fprintf(w, "\n")
	}

	w.Flush()

}

func main() {
	if len(os.Args) < 2 {
		usage()
	}

	TEAMCITY_TOKEN = os.Getenv("TEAMCITY_TOKEN")
	TEAMCITY_HOST = os.Getenv("TEAMCITY_HOST")

	if TEAMCITY_TOKEN == "" {
		fmt.Fprintf(os.Stderr, "TEAMCITY_TOKEN not defined\n")
	}
	if TEAMCITY_HOST == "" {
		fmt.Fprintf(os.Stderr, "TEAMCITY_HOST not defined\n")
	}
	if TEAMCITY_TOKEN == "" || TEAMCITY_HOST == "" {
		os.Exit(1)
	}

	switch os.Args[1] {
	case "status":
		if len(os.Args) > 2 {
			getBuildStatus(os.Args[2])
		} else {
			getBuildStatusAll()
		}

	case "summary":
		getBuildStatusSummary()

	case "buildtypes":
		v := getBuildTypes()
		for _, s := range v {
			fmt.Printf("%s\n", s)
		}

	case "log":
		verbose := 0
		logarg := ""
		for i := 2; i < len(os.Args); i++ {
			if strings.HasPrefix(os.Args[i], "-v") {
				verbose += len(os.Args[i]) - 1
			} else {
				logarg = os.Args[i]
			}
		}
		if logarg == "" {
			usage()
		}
		buildId, err := strconv.Atoi(logarg)
		var logbody io.Reader
		if err == nil {
			logbody = downloadLog(buildId)
		} else {
			logbody, err = os.Open(logarg)
			must(err)
		}
		cleanupLog(logbody, verbose)

	case "diff":
		diff := getdiff()
		os.Stdout.Write(diff)

	default:
		re := regexp.MustCompile("(?i:" + os.Args[1] + ")")
		bts := []string{}
		for _, bt := range getBuildTypes() {
			if re.MatchString(bt) {
				bts = append(bts, bt)
			}
		}
		if len(bts) == 0 {
			fmt.Fprintf(os.Stderr, "no build types match %s\n", os.Args[1])
			os.Exit(1)
		}

		id := uploadPatch(time.Now().Format(time.RFC3339), getdiff())
		fmt.Printf("Patch uploaded as %s\n", id)

		for _, bt := range bts {
			fmt.Printf("%s ", bt)
			triggerBuild(bt, id)
		}
	}
}
