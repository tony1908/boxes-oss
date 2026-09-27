package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const digitalOceanURL = "https://api.digitalocean.com/v2"

// DigitalOcean implements the DigitalOcean Droplets API v2.
type DigitalOcean struct {
	Token   string
	BaseURL string
	Client  *http.Client
}

type doDroplet struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
	Networks  struct {
		V4 []struct {
			IP   string `json:"ip_address"`
			Type string `json:"type"`
		} `json:"v4"`
	} `json:"networks"`
}

func (p *DigitalOcean) baseURL() string {
	if p.BaseURL == "" {
		return digitalOceanURL
	}
	return strings.TrimRight(p.BaseURL, "/")
}

func (p *DigitalOcean) request(ctx context.Context, method, path string, body any, result any) error {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, p.baseURL()+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+p.Token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := httpClient(p.Client).Do(req)
	if err != nil {
		return fmt.Errorf("digitalocean request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if resp.StatusCode == http.StatusNotFound {
			return ErrNotFound
		}
		return fmt.Errorf("digitalocean API returned HTTP %d", resp.StatusCode)
	}
	if result != nil && resp.StatusCode != http.StatusNoContent {
		if err := json.NewDecoder(resp.Body).Decode(result); err != nil {
			return fmt.Errorf("decode digitalocean response: %w", err)
		}
	}
	return nil
}

func (p *DigitalOcean) Create(ctx context.Context, spec NodeSpec) (Instance, error) {
	body := map[string]any{"name": spec.Name, "region": spec.Region, "size": spec.Size, "image": spec.Image, "user_data": spec.UserData, "tags": spec.Tags}
	if len(spec.SSHKeys) != 0 {
		body["ssh_keys"] = spec.SSHKeys
	}
	var response struct {
		Droplet doDroplet `json:"droplet"`
	}
	if err := p.request(ctx, http.MethodPost, "/droplets", body, &response); err != nil {
		return Instance{}, err
	}
	return convertDODroplet(response.Droplet), nil
}

func (p *DigitalOcean) Get(ctx context.Context, id string) (Instance, error) {
	if err := validateID(id); err != nil {
		return Instance{}, err
	}
	var response struct {
		Droplet doDroplet `json:"droplet"`
	}
	if err := p.request(ctx, http.MethodGet, "/droplets/"+url.PathEscape(id), nil, &response); err != nil {
		return Instance{}, err
	}
	return convertDODroplet(response.Droplet), nil
}

func (p *DigitalOcean) Delete(ctx context.Context, id string) error {
	if err := validateID(id); err != nil {
		return err
	}
	return p.request(ctx, http.MethodDelete, "/droplets/"+url.PathEscape(id), nil, nil)
}

func (p *DigitalOcean) List(ctx context.Context, tag string) ([]Instance, error) {
	instances := make([]Instance, 0)
	for page := 1; ; page++ {
		query := url.Values{"tag_name": {tag}, "per_page": {"200"}, "page": {strconv.Itoa(page)}}
		var response struct {
			Droplets []doDroplet `json:"droplets"`
			Links    struct {
				Pages struct {
					Next string `json:"next"`
				} `json:"pages"`
			} `json:"links"`
		}
		if err := p.request(ctx, http.MethodGet, "/droplets?"+query.Encode(), nil, &response); err != nil {
			return nil, err
		}
		for _, droplet := range response.Droplets {
			instances = append(instances, convertDODroplet(droplet))
		}
		if response.Links.Pages.Next == "" {
			break
		}
	}
	return instances, nil
}

func (p *DigitalOcean) CreateImage(ctx context.Context, instanceID, name string) (string, error) {
	if err := validateID(instanceID); err != nil {
		return "", err
	}
	var response struct {
		Action struct {
			ID int64 `json:"id"`
		} `json:"action"`
	}
	if err := p.request(ctx, http.MethodPost, "/droplets/"+url.PathEscape(instanceID)+"/actions", map[string]string{"type": "snapshot", "name": name}, &response); err != nil {
		return "", err
	}
	if response.Action.ID <= 0 {
		return "", fmt.Errorf("digitalocean snapshot action returned an invalid ID")
	}
	for {
		var action struct {
			Action struct {
				Status string `json:"status"`
			} `json:"action"`
		}
		if err := p.request(ctx, http.MethodGet, "/actions/"+strconv.FormatInt(response.Action.ID, 10), nil, &action); err != nil {
			return "", err
		}
		switch strings.ToLower(action.Action.Status) {
		case "completed":
			goto findSnapshot
		case "errored":
			return "", fmt.Errorf("digitalocean snapshot action %d errored", response.Action.ID)
		}
		timer := time.NewTimer(10 * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return "", ctx.Err()
		case <-timer.C:
		}
	}

findSnapshot:
	var snapshots struct {
		Snapshots []struct {
			ID        int64     `json:"id"`
			Name      string    `json:"name"`
			CreatedAt time.Time `json:"created_at"`
		} `json:"snapshots"`
	}
	if err := p.request(ctx, http.MethodGet, "/droplets/"+url.PathEscape(instanceID)+"/snapshots", nil, &snapshots); err != nil {
		return "", err
	}
	var newest *struct {
		ID        int64     `json:"id"`
		Name      string    `json:"name"`
		CreatedAt time.Time `json:"created_at"`
	}
	for i := range snapshots.Snapshots {
		candidate := &snapshots.Snapshots[i]
		if candidate.Name == name && candidate.ID > 0 && (newest == nil || candidate.CreatedAt.After(newest.CreatedAt)) {
			newest = candidate
		}
	}
	if newest == nil {
		return "", fmt.Errorf("digitalocean snapshot %q not found for droplet %s", name, instanceID)
	}
	return strconv.FormatInt(newest.ID, 10), nil
}
func (p *DigitalOcean) DeleteImage(ctx context.Context, id string) error {
	if err := validateID(id); err != nil {
		return err
	}
	return p.request(ctx, http.MethodDelete, "/images/"+url.PathEscape(id), nil, nil)
}
func (p *DigitalOcean) ListImagesByName(ctx context.Context, name string) ([]string, error) {
	var ids []string
	page := 1
	for {
		query := url.Values{"private": {"true"}, "per_page": {"200"}, "page": {strconv.Itoa(page)}}
		var response struct {
			Images []struct {
				ID   int64  `json:"id"`
				Name string `json:"name"`
			} `json:"images"`
			Links struct {
				Pages struct {
					Next string `json:"next"`
				} `json:"pages"`
			} `json:"links"`
		}
		if err := p.request(ctx, http.MethodGet, "/images?"+query.Encode(), nil, &response); err != nil {
			return nil, err
		}
		for _, image := range response.Images {
			if image.Name == name {
				ids = append(ids, strconv.FormatInt(image.ID, 10))
			}
		}
		if response.Links.Pages.Next == "" {
			return ids, nil
		}
		page++
	}
}
func (p *DigitalOcean) ImageStatus(ctx context.Context, id string) (string, error) {
	var response struct {
		Image struct {
			Status string `json:"status"`
		} `json:"image"`
	}
	if err := p.request(ctx, http.MethodGet, "/images/"+url.PathEscape(id), nil, &response); err != nil {
		return "", err
	}
	return response.Image.Status, nil
}

func convertDODroplet(d doDroplet) Instance {
	instance := Instance{ID: strconv.FormatInt(d.ID, 10), Name: d.Name, Status: d.Status, CreatedAt: d.CreatedAt}
	for _, network := range d.Networks.V4 {
		if network.Type == "public" {
			instance.PublicIPv4 = network.IP
			break
		}
	}
	return instance
}
