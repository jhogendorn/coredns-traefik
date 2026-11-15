package traefik

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

const apiTimeout = 10 * time.Second

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
	result := &[]HttpRouter{}
	if err := fetchRouters(c.client, c.httpRoutersUrl, result); err != nil {
		return nil, err
	}
	return result, nil
}

func (c *TraefikClient) GetTcpRouters() (*[]TcpRouter, error) {
	result := &[]TcpRouter{}
	if err := fetchRouters(c.client, c.tcpRoutersUrl, result); err != nil {
		return nil, err
	}
	return result, nil
}

func fetchRouters(client *http.Client, routersUrl string, out interface{}) error {
	log.Debugf("Connecting to %s", routersUrl)
	response, err := client.Get(routersUrl)

	if err != nil {
		log.Errorf("Failed to fetch routers: %q", err)
		return err
	}

	var body []byte
	var readErr error
	if response.Body != nil {
		defer response.Body.Close()
		body, readErr = io.ReadAll(response.Body)
	}

	if readErr != nil {
		log.Errorf("Failed to read response body: %q", readErr)
		return readErr
	}

	if response.StatusCode != 200 {
		if body != nil {
			bodyStr := string(body[:])
			return fmt.Errorf("Received %d response from API: %s", response.StatusCode, bodyStr)
		} else {
			return fmt.Errorf("Received %d response from API", response.StatusCode)
		}
	}

	if body != nil {
		if err := json.Unmarshal(body, out); err != nil {
			return fmt.Errorf("Failed to parse json body: %q", err)
		}
	}

	return nil
}
