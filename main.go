package main

import (
	"os"
	"net/http"
	"fmt"
	"io/ioutil"
	"bytes"
	"os/exec"
	"strings"
	"time"
	"encoding/json"
	"io"
	"text/tabwriter"
)

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

func uploadPatch(buildName string, diff []byte) string {
	req, err := http.NewRequest("POST", fmt.Sprintf("https://%s/uploadDiffChanges.html?description=%s&commitType=0", TEAMCITY_HOST, buildName), bytes.NewReader(diff))
	must(err)
	req.Header.Add("Authorization", fmt.Sprintf("Bearer %s", TEAMCITY_TOKEN))
	req.Header.Add("Content-Type", "text/text")
	req.Header.Add("Origin", TEAMCITY_HOST)
	resp, err := http.DefaultClient.Do(req)
	must(err)
	//fmt.Printf("%#v\n", resp)
	buf, err := ioutil.ReadAll(resp.Body)
	must(err)
	return strings.TrimSpace(string(buf))
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
	//fmt.Printf("%s\n", build)
	req, err := http.NewRequest("POST", fmt.Sprintf("https://%s/app/rest/buildQueue", TEAMCITY_HOST), bytes.NewReader(build))
	must(err)
	req.Header.Add("Authorization", fmt.Sprintf("Bearer %s", TEAMCITY_TOKEN))
	req.Header.Add("Content-Type", "application/xml")
	req.Header.Add("Accept", "application/json")
	req.Header.Add("Origin", TEAMCITY_HOST)
	resp, err := http.DefaultClient.Do(req)
	must(err)
	//fmt.Printf("%#v\n", resp)
	buf, err := ioutil.ReadAll(resp.Body)
	must(err)
	//fmt.Printf("%s\n", string(buf))
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
	Id int
	BuildTypeId string
	State string
	Status string
	StatusText string
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
	req, err := http.NewRequest("GET", fmt.Sprintf("https://%s/app/rest/builds/id:%s", TEAMCITY_HOST, buildId), nil)
	must(err)
	req.Header.Add("Authorization", fmt.Sprintf("Bearer %s", TEAMCITY_TOKEN))
	req.Header.Add("Content-Type", "application/json")
	req.Header.Add("Accept", "application/json")
	req.Header.Add("Origin", TEAMCITY_HOST)
	resp, err := http.DefaultClient.Do(req)
	must(err)
	//fmt.Printf("%#v\n", resp)
	buf, err := ioutil.ReadAll(resp.Body)
	must(err)
	//fmt.Printf("%s\n", string(buf))
	bs := decodeBuildStatus(bytes.NewReader(buf))
	w := tabwriter.NewWriter(os.Stdout, 8, 8, 1, ' ', 0)
	defer w.Flush()
	fmt.Fprintf(w, "URL:\t%s\n", bs.URL())
	fmt.Fprintf(w, "Build Type:\t%s\n", bs.BuildTypeId)
	fmt.Fprintf(w, "State:\t%s\n", bs.State)
	fmt.Fprintf(w, "Status:\t%s\n", bs.Status)
	fmt.Fprintf(w, "Text:\t%s\n", bs.StatusText)
}

func usage() {
	fmt.Fprintf(os.Stderr, `Usage:
	
	teamcityrun diff			shows current diff
	teamcityrun all			runs current diff on all architecture combinations
	teamcityrun os/arch/ver		runs current diff on the specified platform
	teamcityrun status build-id	shows status of build
	
Example:
	teamcityrun Windows/amd64/1.17
`)
	os.Exit(1)
}

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	
	
	TEAMCITY_TOKEN = os.Getenv("TEAMCITY_TOKEN")
	TEAMCITY_HOST = os.Getenv("TEAMCITY_HOST")
	
	switch os.Args[1] {
	case "status":
		getBuildStatus(os.Args[2])
	
	case "diff":
		diff := getdiff()
		os.Stdout.Write(diff)
	case "all":
		// TODO: implement
	default:
		v := strings.Split(os.Args[1], "/")
		if len(v) != 3 {
			fmt.Fprintf(os.Stderr, "wrong platform format")
			usage()
		}
		os, arch, gover := v[0], v[1], v[2]
		gover = strings.ReplaceAll(gover, ".", "_")
		
		id := uploadPatch(time.Now().Format(time.RFC3339), getdiff())
		fmt.Printf("Patch uploaded at %s\n", id)
		id = "948"
		triggerBuild(fmt.Sprintf("Delve_%s_%s_%s", os, arch, gover), id)
		
		// Or Delve_AggregatorBuild
	}

	
	
	//TODO:
	// - return list of build configurations (app/rest/buildTypes)
	// - trigger build and return build location
	// - trigger build for aggregator configurations
	// - download and clean build log
}
