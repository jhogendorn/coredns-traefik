package traefik

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

const (
	apiTimeout = 10 * time.Second
	pageSize   = 100
	maxPages   = 100
)

type ITraefikClient interface {
	GetHttpRouters() (*[]HttpRouter, error)
	GetTcpRouters() (*[]TcpRouter, error)
}

type TraefikClient struct {
	ITraefikClient
	httpRoutersUrl string
	tcpRoutersUrl  string
	config         *TraefikConfig
	client         *http.Client
}

func NewTraefikClient(cfg *TraefikConfig) (*TraefikClient, error) {
	httpRoutersUrl, err := url.JoinPath(cfg.baseUrl.String(), "/http/routers")
	if err != nil {
		return nil, err
	}

	tcpRoutersUrl, err := url.JoinPath(cfg.baseUrl.String(), "/tcp/routers")
	if err != nil {
		return nil, err
	}

	client := &TraefikClient{
		httpRoutersUrl: httpRoutersUrl,
		tcpRoutersUrl:  tcpRoutersUrl,
		config:         cfg,
	}

	if cfg.insecureSkipVerify {
		tr := &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		}
		client.client = &http.Client{Transport: tr, Timeout: apiTimeout}
	} else {
		client.client = &http.Client{Timeout: apiTimeout}
	}

	return client, nil
}

func (c *TraefikClient) GetHttpRouters() (*[]HttpRouter, error) {
	result := []HttpRouter{}
	err := fetchPages(c.client, c.httpRoutersUrl, func(body []byte) error {
		page := []HttpRouter{}
		if err := json.Unmarshal(body, &page); err != nil {
			return err
		}
		result = append(result, page...)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &result, nil
}

func (c *TraefikClient) GetTcpRouters() (*[]TcpRouter, error) {
	result := []TcpRouter{}
	err := fetchPages(c.client, c.tcpRoutersUrl, func(body []byte) error {
		page := []TcpRouter{}
		if err := json.Unmarshal(body, &page); err != nil {
			return err
		}
		result = append(result, page...)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &result, nil
}

// Traefik serves at most per_page items per request and names the next page in
// X-Next-Page, which is 1 (or absent) on the last page.
func fetchPages(client *http.Client, routersUrl string, onPage func(body []byte) error) error {
	for page := 1; page <= maxPages; {
		body, next, err := fetchPage(client, fmt.Sprintf("%s?page=%d&per_page=%d", routersUrl, page, pageSize))
		if err != nil {
			return err
		}

		if body != nil {
			if err := onPage(body); err != nil {
				return fmt.Errorf("Failed to parse json body: %q", err)
			}
		}

		if next == "" || next == "1" {
			return nil
		}

		nextPage, err := strconv.Atoi(next)
		if err != nil || nextPage <= page {
			return fmt.Errorf("Invalid X-Next-Page %q after page %d", next, page)
		}
		page = nextPage
	}

	return fmt.Errorf("More than %d pages of routers at %s", maxPages, routersUrl)
}

func fetchPage(client *http.Client, pageUrl string) ([]byte, string, error) {
	log.Debugf("Connecting to %s", pageUrl)
	response, err := client.Get(pageUrl)

	if err != nil {
		log.Errorf("Failed to fetch routers: %q", err)
		return nil, "", err
	}

	var body []byte
	var readErr error
	if response.Body != nil {
		defer response.Body.Close()
		body, readErr = io.ReadAll(response.Body)
	}

	if readErr != nil {
		log.Errorf("Failed to read response body: %q", readErr)
		return nil, "", readErr
	}

	if response.StatusCode != 200 {
		if body != nil {
			bodyStr := string(body[:])
			return nil, "", fmt.Errorf("Received %d response from API: %s", response.StatusCode, bodyStr)
		} else {
			return nil, "", fmt.Errorf("Received %d response from API", response.StatusCode)
		}
	}

	return body, response.Header.Get("X-Next-Page"), nil
}
