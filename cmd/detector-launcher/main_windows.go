//go:build windows

package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const home = "https://lirahub.dpdns.org/modeltrace/"

func detectorURL(raw string) (string, error) {
	target, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || target.Hostname() == "" || (target.Scheme != "https" && target.Scheme != "http") ||
		target.User != nil || target.ForceQuery || target.RawQuery != "" || target.Fragment != "" {
		return "", errors.New("检测地址需为不含查询参数、片段或登录信息的 HTTP(S) URL。")
	}
	return strings.TrimRight(target.String(), "/") + "/", nil
}

func checkPage(url string) error {
	c := http.Client{Timeout: 8 * time.Second, Transport: &http.Transport{Proxy: http.ProxyFromEnvironment, DisableKeepAlives: true}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	for _, path := range []string{"healthz", ""} {
		r, err := c.Get(url + path)
		if err != nil {
			return fmt.Errorf("无法连接检测器：%w", err)
		}
		var health struct {
			Status  string
			Version string
		}
		if path == "healthz" {
			err = json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&health)
		}
		r.Body.Close()
		if r.StatusCode != 200 {
			return fmt.Errorf("检测器访问 /%s 返回 HTTP %d", path, r.StatusCode)
		}
		if path == "healthz" && (err != nil || health.Status != "ok" || health.Version == "") {
			return errors.New("检测器健康接口未返回有效版本。")
		}
	}
	return nil
}

func openPage(target string, check bool) error {
	if err := checkPage(target); err != nil {
		return err
	}
	if check {
		return nil
	}
	return launchBrowser(target)
}

func main() {
	check := flag.Bool("check", false, "检查检测页，不打开浏览器")
	address := flag.String("url", home, "检测页地址")
	flag.Parse()
	target, err := detectorURL(*address)
	if err == nil {
		err = openPage(target, *check)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		if !*check {
			showError(err.Error())
		}
		os.Exit(1)
	}
	if *check {
		fmt.Println("Detector is ready at " + target)
	}
}
