package client

import (
	"fmt"
	"net/http"
	"net/url"
)

func build(requestID string) (*http.Request, error) {
	target := fmt.Sprintf("/charge/%s?view=full", url.PathEscape(requestID))
	return http.NewRequest(http.MethodGet, target, nil)
}

func send(request *http.Request) (*http.Response, error) {
	return http.DefaultClient.Do(request)
}

func Call(requestID string) {
	request, _ := build(requestID)
	_, _ = send(request)
}

type API struct {
	baseURL string
}

func (api *API) UnknownAuthority(requestID string) {
	_, _ = http.Get(api.baseURL + "/charge/" + url.PathEscape(requestID))
}

func External() {
	_, _ = http.Get("https://api.example.test/health")
}

func Ambiguous(requestID string) {
	request, _ := http.NewRequest(http.MethodGet, "/ambiguous/"+url.PathEscape(requestID), nil)
	_, _ = http.DefaultClient.Do(request)
}

func Unsafe(requestID string) {
	_, _ = http.Get(fmt.Sprintf("/unsafe/%s", requestID))
}
