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

const hetznerURL = "https://api.hetzner.cloud/v1"

// Hetzner implements the Hetzner Cloud servers API.
type Hetzner struct {
	Token   string
	BaseURL string
	Client  *http.Client
}

type hetznerServer struct {
	ID        int64             `json:"id"`
	Name      string            `json:"name"`
	Status    string            `json:"status"`
	Created   time.Time         `json:"created"`
	Labels    map[string]string `json:"labels"`
	PublicNet struct {
		IPv4 struct {
			IP string `json:"ip"`
		} `json:"ipv4"`
	} `json:"public_net"`
}

func (p *Hetzner) baseURL() string {
	if p.BaseURL == "" {
		return hetznerURL
	}
	return strings.TrimRight(p.BaseURL, "/")
}

func (p *Hetzner) request(ctx context.Context, method, path string, body any, result any) error {
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
		return fmt.Errorf("hetzner request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if resp.StatusCode == http.StatusNotFound {
			return ErrNotFound
		}
		return fmt.Errorf("hetzner API returned HTTP %d", resp.StatusCode)
	}
	if result != nil && resp.StatusCode != http.StatusNoContent {
		if err := json.NewDecoder(resp.Body).Decode(result); err != nil {
			return fmt.Errorf("decode hetzner response: %w", err)
		}
	}
	return nil
}

func (p *Hetzner) Create(ctx context.Context, spec NodeSpec) (Instance, error) {
	labels := make(map[string]string)
	for _, tag := range spec.Tags {
		if strings.HasPrefix(tag, "boxes-pool-") {
			labels["boxes-pool"] = strings.TrimPrefix(tag, "boxes-pool-")
		}
	}
	body := map[string]any{"name": spec.Name, "location": spec.Region, "server_type": spec.Size, "image": spec.Image, "user_data": spec.UserData, "labels": labels}
	if len(spec.SSHKeys) != 0 {
		body["ssh_keys"] = spec.SSHKeys
	}
	var response struct {
		Server hetznerServer `json:"server"`
	}
	if err := p.request(ctx, http.MethodPost, "/servers", body, &response); err != nil {
		return Instance{}, err
	}
	return convertHetznerServer(response.Server), nil
}

func (p *Hetzner) Get(ctx context.Context, id string) (Instance, error) {
	if err := validateID(id); err != nil {
		return Instance{}, err
	}
	var response struct {
		Server hetznerServer `json:"server"`
	}
	if err := p.request(ctx, http.MethodGet, "/servers/"+url.PathEscape(id), nil, &response); err != nil {
		return Instance{}, err
	}
	return convertHetznerServer(response.Server), nil
}

func (p *Hetzner) Delete(ctx context.Context, id string) error {
	if err := validateID(id); err != nil {
		return err
	}
	return p.request(ctx, http.MethodDelete, "/servers/"+url.PathEscape(id), nil, nil)
}

func (p *Hetzner) List(ctx context.Context, tag string) ([]Instance, error) {
	label, value, ok := strings.Cut(strings.TrimPrefix(tag, "boxes-pool-"), "=")
	if ok { // Explicit label selector callers may provide key=value.
		tag = label + "=" + value
	} else if strings.HasPrefix(tag, "boxes-pool-") {
		tag = "boxes-pool=" + strings.TrimPrefix(tag, "boxes-pool-")
	}
	instances := make([]Instance, 0)
	for page := 1; ; page++ {
		query := url.Values{"label_selector": {tag}, "per_page": {"50"}, "page": {strconv.Itoa(page)}}
		var response struct {
			Servers []hetznerServer `json:"servers"`
			Meta    struct {
				Pagination struct {
					NextPage *int `json:"next_page"`
				} `json:"pagination"`
			} `json:"meta"`
		}
		if err := p.request(ctx, http.MethodGet, "/servers?"+query.Encode(), nil, &response); err != nil {
			return nil, err
		}
		for _, server := range response.Servers {
			instances = append(instances, convertHetznerServer(server))
		}
		if response.Meta.Pagination.NextPage == nil {
			break
		}
		page = *response.Meta.Pagination.NextPage - 1
	}
	return instances, nil
}

func (p *Hetzner) CreateImage(ctx context.Context, instanceID, name string) (string, error) {
	if err := validateID(instanceID); err != nil {
		return "", err
	}
	var response struct {
		Image struct {
			ID int64 `json:"id"`
		} `json:"image"`
	}
	body := map[string]string{"type": "snapshot", "description": name}
	if err := p.request(ctx, http.MethodPost, "/servers/"+url.PathEscape(instanceID)+"/actions/create_image", body, &response); err != nil {
		return "", err
	}
	return strconv.FormatInt(response.Image.ID, 10), nil
}
func (p *Hetzner) DeleteImage(ctx context.Context, id string) error {
	if err := validateID(id); err != nil {
		return err
	}
	return p.request(ctx, http.MethodDelete, "/images/"+url.PathEscape(id), nil, nil)
}
func (p *Hetzner) ListImagesByName(ctx context.Context, name string) ([]string, error) {
	var ids []string
	page := 1
	for {
		var response struct {
			Images []struct {
				ID          int64  `json:"id"`
				Description string `json:"description"`
			} `json:"images"`
			Meta struct {
				Pagination struct {
					NextPage *int `json:"next_page"`
				} `json:"pagination"`
			} `json:"meta"`
		}
		if err := p.request(ctx, http.MethodGet, "/images?type=snapshot&page="+strconv.Itoa(page), nil, &response); err != nil {
			return nil, err
		}
		for _, image := range response.Images {
			if image.Description == name {
				ids = append(ids, strconv.FormatInt(image.ID, 10))
			}
		}
		if response.Meta.Pagination.NextPage == nil {
			return ids, nil
		}
		page = *response.Meta.Pagination.NextPage
	}
}
func (p *Hetzner) ImageStatus(ctx context.Context, id string) (string, error) {
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

func convertHetznerServer(s hetznerServer) Instance {
	return Instance{ID: strconv.FormatInt(s.ID, 10), Name: s.Name, PublicIPv4: s.PublicNet.IPv4.IP, Status: s.Status, CreatedAt: s.Created}
}
