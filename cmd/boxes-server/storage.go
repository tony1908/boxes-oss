package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func runStorageSet(args []string, adminURL, accessKey, secretKey string, output io.Writer) error {
	flags := flag.NewFlagSet("storage set", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	dataDir := flags.String("data-dir", "/var/lib/boxes", "server data directory")
	endpoint := flags.String("s3-endpoint", "", "S3 endpoint URL")
	bucket := flags.String("s3-bucket", "", "S3 bucket name")
	prefix := flags.String("s3-prefix", "boxes", "S3 repository prefix")
	region := flags.String("s3-region", "", "AWS-compatible region")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		return errors.New("usage: boxes-server storage set [--data-dir DIR] --s3-endpoint URL --s3-bucket NAME [--s3-prefix P] [--s3-region R]")
	}
	if *endpoint == "" || *bucket == "" || accessKey == "" || secretKey == "" {
		return errors.New("S3 endpoint and bucket flags plus BOXES_S3_ACCESS_KEY and BOXES_S3_SECRET_KEY environment variables are required")
	}
	token, err := os.ReadFile(filepath.Join(*dataDir, "admin.token"))
	if err != nil {
		return fmt.Errorf("could not read admin token from %s: %w", filepath.Join(*dataDir, "admin.token"), err)
	}
	adminToken := strings.TrimSpace(string(token))
	if adminToken == "" {
		return errors.New("admin token file is empty")
	}
	parsed, err := url.Parse(adminURL)
	if err != nil || parsed.Scheme != "http" || parsed.Hostname() == "" || parsed.User != nil {
		return errors.New("server admin endpoint must be a local HTTP URL")
	}
	address := parsed.Hostname()
	if address != "localhost" {
		ip := net.ParseIP(address)
		if ip == nil || !ip.IsLoopback() {
			return errors.New("server admin endpoint must resolve to localhost")
		}
	}
	payload, err := json.Marshal(map[string]string{
		"endpoint": *endpoint, "bucket": *bucket, "prefix": *prefix, "region": *region,
		"accessKey": accessKey, "secretKey": secretKey,
	})
	if err != nil {
		return errors.New("could not encode S3 storage settings")
	}
	request, err := http.NewRequest(http.MethodPost, adminURL, strings.NewReader(string(payload)))
	if err != nil {
		return errors.New("could not create local storage settings request")
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+adminToken)
	client := &http.Client{Timeout: 30 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		return errors.New("could not update snapshot storage; is boxes-server running locally?")
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("could not update snapshot storage (server returned HTTP %d)", response.StatusCode)
	}
	var result struct {
		Type     string `json:"type"`
		Endpoint string `json:"endpoint"`
		Bucket   string `json:"bucket"`
		Prefix   string `json:"prefix"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 4096)).Decode(&result); err != nil {
		return errors.New("server returned invalid storage settings")
	}
	return json.NewEncoder(output).Encode(result)
}
