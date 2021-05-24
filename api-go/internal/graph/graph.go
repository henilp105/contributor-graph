package graph

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/datastore"
	"cloud.google.com/go/storage"
	"github.com/api7/contributor-graph/api/internal/utils"
)

// base on experiments :(
var minSuccessfulSVGLen = 8000

func GenerateAndSaveSVG(ctx context.Context, repo string, merge bool) (string, error) {
	bucket := "api7-301102.appspot.com"
	object := utils.RepoNameToFileName(repo, merge) + ".svg"

	client, err := storage.NewClient(ctx)
	if err != nil {
		return "", fmt.Errorf("upload svg failed: storage.NewClient: %v", err)
	}
	defer client.Close()

	graphFunctionUrl := "https://cloudfunction.contributor-graph.com/svg?repo=" + repo
	if merge {
		graphFunctionUrl += "&merge=true"
	}
	resp, err := http.Get(graphFunctionUrl)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		// add a simple retry
		fmt.Println("Oops something went wrong when getting svg. Retry now.")
		resp, err = http.Get(graphFunctionUrl)
		if err != nil {
			return "", err
		}
		if resp.StatusCode != http.StatusOK {
			return "", fmt.Errorf("get svg failed with %d", resp.StatusCode)
		}
	}
	defer resp.Body.Close()
	svg, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	if ok, err := svgSucceed(string(svg[:])); !ok {
		fmt.Println("Oops something went wrong. Retry now.")
		if err != nil {
			fmt.Printf("Error: %s\n", err.Error())
		}
		fmt.Println(graphFunctionUrl)
		resp, err = http.Get(graphFunctionUrl)
		if err != nil {
			return "", err
		}
		defer resp.Body.Close()
		svg, err = ioutil.ReadAll(resp.Body)
		if err != nil {
			return "", err
		}
		if ok, err := svgSucceed(string(svg[:])); !ok {
			return "", fmt.Errorf("get svg failed since %s", err.Error())
		}
	}

	wc := client.Bucket(bucket).Object(object).NewWriter(ctx)
	wc.CacheControl = "public, max-age=86400"
	wc.ContentType = "image/svg+xml;charset=utf-8"

	if _, err = io.Copy(wc, bytes.NewReader(svg)); err != nil {
		return "", fmt.Errorf("upload svg failed: io.Copy: %v", err)
	}
	if err := wc.Close(); err != nil {
		return "", fmt.Errorf("upload svg failed: Writer.Close: %v", err)
	}

	fmt.Printf("New SVG generated with %s\n", repo)

	return string(svg[:]), nil
}

func SubGetSVG(w http.ResponseWriter, repo string, merge bool) (string, error) {
	bucket := "api7-301102.appspot.com"
	object := utils.RepoNameToFileName(repo, merge) + ".svg"

	ctx := context.Background()
	client, err := storage.NewClient(ctx)
	if err != nil {
		return "", err
	}
	reader, err := client.Bucket(bucket).Object(object).NewReader(ctx)
	if err != nil {
		return "", err
	}
	LastModifiedTime, err := reader.LastModified()
	if err != nil {
		return "", err
	}
	// if the svg is too small (<2kb), or graph is outdated, do the update.
	// TODO: Something wrong with last modified time
	if reader.Size() < 2000 || LastModifiedTime.Add(48*time.Hour).Before(time.Now()) {
		fmt.Println(reader.Size(), LastModifiedTime)
		return "", utils.ErrSVGNeedUpdate
	}

	svg, err := ioutil.ReadAll(reader)
	reader.Close()
	if err != nil {
		return "", err
	}

	dbCli, err := datastore.NewClient(ctx, utils.ProjectID)
	if err != nil {
		return "", fmt.Errorf("Failed to create client: %v", err)
	}
	defer dbCli.Close()

	// note, to record traffic, we need to pay 15 RMB per 1M click
	// if we want to cut off this payment, we could toss a dice here and do the record in a certain probability
	// when people really put the image in their README/website, since the click times is a lot
	// we could still tell if people are using it
	storeName := repo
	if merge {
		storeName = "merge-" + repo
	}
	key := datastore.NameKey("GraphTraffic", storeName, nil)
	traffic := utils.GraphTraffic{}
	err = dbCli.Get(ctx, key, &traffic)
	if err != nil {
		if err != datastore.ErrNoSuchEntity {
			return "", err
		}
	}
	if _, err = dbCli.Put(ctx, key, &utils.GraphTraffic{traffic.Num + 1, time.Now()}); err != nil {
		return "", err
	}

	return string(svg), nil
}

// Since currently front-end can not give concise time svg got rendered,
// we need to also tell if the graph is ready to use on this side.
// Try to get the endpoint of the line drawn and tell if it's on the right-most side
func svgSucceed(svg string) (bool, error) {
	lines := strings.Split(svg, "\n")
	var svgWidth int
	for _, l := range lines {
		if strings.Contains(l, "<rect") {
			words := strings.Split(l, " ")
			for _, w := range words {
				if strings.Contains(w, "width") {
					parts := strings.Split(w, `"`)
					var err error
					svgWidth, err = strconv.Atoi(parts[1])
					if err != nil {
						return false, err
					}
					break
				}
			}
		}
	}
	if svgWidth == 0 {
		return false, fmt.Errorf("could not get svg width")
	}
	lineColor := "39a85a"
	for _, l := range lines {
		if strings.Contains(l, lineColor) {
			lineDrawn := strings.Split(strings.Split(l, `"`)[1], " ")
			endPointX, err := strconv.Atoi(lineDrawn[len(lineDrawn)-2])
			if err != nil {
				return false, err
			}
			if float64(endPointX) > 0.95*float64(svgWidth) {
				return true, nil
			}
			return false, fmt.Errorf("the line is not reach its end")
		}
	}
	return false, fmt.Errorf("could not get endpoint")
}
