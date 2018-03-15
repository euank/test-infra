/*
Copyright 2017 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"bufio"
	"encoding/csv"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/go-openapi/spec"
	"github.com/golang/glog"
)

var (
	openAPIFile       = flag.String("openapi", "https://raw.githubusercontent.com/kubernetes/kubernetes/master/api/openapi-spec/swagger.json", "URL to openapi-spec of Kubernetes")
	outputCoveredAPIs = flag.Bool("output-covered-apis", false, "Output the list of covered APIs")
	minCoverage       = flag.Int("minimum-coverage", 0, "This command fails if the number of covered APIs is less than this option ratio(percent)")
	restLog           = flag.String("restlog", "", "File path to REST API operation log of Kubernetes")
)

type apiData struct {
	Method string
	URL    string
}

type apiArray []apiData

func parseOpenAPI(rawdata []byte) apiArray {
	var swaggerSpec spec.Swagger
	var apisOpenapi apiArray

	err := swaggerSpec.UnmarshalJSON(rawdata)
	if err != nil {
		log.Fatal(err)
	}

	for path, pathItem := range swaggerSpec.Paths.Paths {
		// Some paths contain "/" at the end of swagger spec, here removes "/" for comparing them easily later.
		path = strings.TrimRight(path, "/")

		// Standard HTTP methods: https://github.com/OAI/OpenAPI-Specification/blob/master/versions/2.0.md#path-item-object
		methods := []string{"get", "put", "post", "delete", "options", "head", "patch"}
		for _, method := range methods {
			methodSpec, err := pathItem.JSONLookup(method)
			if err != nil {
				log.Fatal(err)
			}
			t, ok := methodSpec.(*spec.Operation)
			if ok == false {
				log.Fatal("Failed to convert methodSpec.")
			}
			if t == nil {
				continue
			}
			method := strings.ToUpper(method)
			api := apiData{
				Method: method,
				URL:    path,
			}
			apisOpenapi = append(apisOpenapi, api)
		}
	}
	return apisOpenapi
}

func getOpenAPISpec(url string) apiArray {
	resp, err := http.Get(url)
	if err != nil {
		log.Fatal(err)
	}
	bytes, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		log.Fatal(err)
	}
	return parseOpenAPI(bytes)
}

//   I0919 15:34:14.943642    6611 round_trippers.go:414] GET https://172.27.138.63:6443/api/v1/namespaces/kube-system/replicationcontrollers
var reAPILog = regexp.MustCompile(`round_trippers.go:\d+\] (GET|PUT|POST|DELETE|OPTIONS|HEAD|PATCH) (\S+)`)

func parseAPILog(fp io.Reader) apiArray {
	var apisLog apiArray
	var err error

	reader := bufio.NewReaderSize(fp, 4096)
	for line := ""; err == nil; line, err = reader.ReadString('\n') {
		result := reAPILog.FindSubmatch([]byte(line))
		if len(result) == 0 {
			continue
		}
		method := strings.ToUpper(string(result[1]))
		rawurl := string(result[2])
		parsedURL, err := url.Parse(rawurl)
		if err != nil {
			log.Fatal(err)
		}
		api := apiData{
			Method: method,
			URL:    parsedURL.Path,
		}
		apisLog = append(apisLog, api)
	}
	return apisLog
}

func getAPILog(restlog string) apiArray {
	var fp *os.File
	var err error

	fp, err = os.Open(restlog)
	if err != nil {
		log.Fatal(err)
	}
	defer fp.Close()

	return parseAPILog(fp)
}

var reOpenapi = regexp.MustCompile(`({\S+?})`)

func getTestedAPIs(apisOpenapi, apisLogs apiArray) apiArray {
	var found bool
	var apisTested apiArray

	for _, openapi := range apisOpenapi {
		regURL := reOpenapi.ReplaceAllLiteralString(openapi.URL, `[^/\s]+`) + `$`
		reg := regexp.MustCompile(regURL)
		found = false
		for _, log := range apisLogs {
			if openapi.Method != log.Method {
				continue
			}
			if !reg.MatchString(log.URL) {
				continue
			}
			found = true
			apisTested = append(apisTested, openapi)
			break
		}
		if found {
			continue
		}
	}
	return apisTested
}

func getTestedAPIsByLevel(negative bool, reg *regexp.Regexp, apisOpenapi, apisTested apiArray) (apiArray, apiArray) {
	var apisTestedByLevel apiArray
	var apisAllByLevel apiArray

	for _, openapi := range apisTested {
		if (negative == false && reg.MatchString(openapi.URL)) ||
			(negative == true && !reg.MatchString(openapi.URL)) {
			apisTestedByLevel = append(apisTestedByLevel, openapi)
		}
	}
	for _, openapi := range apisOpenapi {
		if (negative == false && reg.MatchString(openapi.URL)) ||
			(negative == true && !reg.MatchString(openapi.URL)) {
			apisAllByLevel = append(apisAllByLevel, openapi)
		}
	}
	return apisTestedByLevel, apisAllByLevel
}

type coverageData struct {
	Total    string
	Tested   string
	Untested string
	Coverage string
}

func getCoverageByLevel(apisTested, apisAll apiArray) coverageData {
	var coverage coverageData

	coverage.Total = fmt.Sprint(len(apisAll))
	coverage.Tested = fmt.Sprint(len(apisTested))
	coverage.Untested = fmt.Sprint(len(apisAll) - len(apisTested))
	coverage.Coverage = fmt.Sprint(100 * len(apisTested) / len(apisAll))

	return coverage
}

//NOTE: This is messy, but the regex doesn't support negative lookahead(?!) on golang.
//This is just a workaround.
var reNotStableAPI = regexp.MustCompile(`\S+(alpha|beta)\S+`)
var reAlphaAPI = regexp.MustCompile(`\S+alpha\S+`)
var reBetaAPI = regexp.MustCompile(`\S+beta\S+`)

func outputCoverage(apisOpenapi, apisTested apiArray) {
	apisTestedByStable, apisAllByStable := getTestedAPIsByLevel(true, reNotStableAPI, apisOpenapi, apisTested)
	apisTestedByAlpha, apisAllByAlpha := getTestedAPIsByLevel(false, reAlphaAPI, apisOpenapi, apisTested)
	apisTestedByBeta, apisAllByBeta := getTestedAPIsByLevel(false, reBetaAPI, apisOpenapi, apisTested)

	coverageAll := getCoverageByLevel(apisTested, apisOpenapi)
	coverageStable := getCoverageByLevel(apisTestedByStable, apisAllByStable)
	coverageAlpha := getCoverageByLevel(apisTestedByAlpha, apisAllByAlpha)
	coverageBeta := getCoverageByLevel(apisTestedByBeta, apisAllByBeta)

	records := [][]string{
		{"API", "TOTAL", "TESTED", "UNTESTED", "COVERAGE(%)"},
		{"ALL", coverageAll.Total, coverageAll.Tested, coverageAll.Untested, coverageAll.Coverage},
		{"STABLE", coverageStable.Total, coverageStable.Tested, coverageStable.Untested, coverageStable.Coverage},
		{"Alpha", coverageAlpha.Total, coverageAlpha.Tested, coverageAlpha.Untested, coverageAlpha.Coverage},
		{"Beta", coverageBeta.Total, coverageBeta.Tested, coverageBeta.Untested, coverageBeta.Coverage},
	}
	w := csv.NewWriter(os.Stdout)
	w.WriteAll(records)

	actualCoverage, _ := strconv.Atoi(coverageAll.Coverage)
	if *minCoverage > int(actualCoverage) {
		log.Fatalf("The API coverage(%d) is lower than the specified one(%d).", actualCoverage, *minCoverage)
	}
}

func main() {
	flag.Parse()
	if len(*restLog) == 0 {
		glog.Fatal("need to set '--restlog'")
	}

	apisOpenapi := getOpenAPISpec(*openAPIFile)
	apisLogs := getAPILog(*restLog)
	apisTested := getTestedAPIs(apisOpenapi, apisLogs)
	outputCoverage(apisOpenapi, apisTested)
	if *outputCoveredAPIs {
		for _, openapi := range apisTested {
			fmt.Printf("%s %s\n", openapi.Method, openapi.URL)
		}
	}
}
