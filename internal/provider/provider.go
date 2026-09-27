// Package provider contains the small cloud API contract used by autoscaling.
package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// NodeSpec describes the provider-specific inputs needed to launch a node.
type NodeSpec struct {
	Name     string   `json:"name"`
	Region   string   `json:"region"`
	Size     string   `json:"size"`
	Image    string   `json:"image"`
	UserData string   `json:"userData"`
	Tags     []string `json:"tags"`
	SSHKeys  []string `json:"sshKeys,omitempty"`
}

// Instance is a provider VM without provider-specific response fields.
type Instance struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	PublicIPv4 string    `json:"publicIPv4,omitempty"`
	Status     string    `json:"status"`
	CreatedAt  time.Time `json:"createdAt,omitempty"`
}

// Provider supports the lifecycle operations needed by an autoscaling pool.
type Provider interface {
	Create(context.Context, NodeSpec) (Instance, error)
	Get(context.Context, string) (Instance, error)
	Delete(context.Context, string) error
	List(context.Context, string) ([]Instance, error)
	CreateImage(context.Context, string, string) (string, error)
	ListImagesByName(context.Context, string) ([]string, error)
	DeleteImage(context.Context, string) error
	ImageStatus(context.Context, string) (string, error)
}

// ErrNotFound reports that the requested instance does not exist.
var ErrNotFound = errors.New("provider instance not found")

func httpClient(client *http.Client) *http.Client {
	if client != nil {
		return client
	}
	return &http.Client{Timeout: 30 * time.Second}
}

func validateID(id string) error {
	if strings.TrimSpace(id) == "" || strings.ContainsAny(id, "/\\\x00\r\n") {
		return errors.New("provider instance ID is invalid")
	}
	return nil
}

// Script executes a user-supplied provider protocol without invoking a shell.
type Script struct {
	Executable string
	Timeout    time.Duration
}

func (p Script) run(ctx context.Context, args []string, input any, output any) error {
	if strings.TrimSpace(p.Executable) == "" {
		return errors.New("script provider executable is required")
	}
	timeout := p.Timeout
	if timeout <= 0 {
		timeout = 2 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, p.Executable, args...)
	if input != nil {
		data, err := json.Marshal(input)
		if err != nil {
			return err
		}
		cmd.Stdin = strings.NewReader(string(data))
	}
	stdout, err := cmd.Output()
	if err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("script provider timed out: %w", ctx.Err())
		}
		return fmt.Errorf("script provider %s failed: %w", args[0], err)
	}
	if output != nil {
		if err := json.Unmarshal(stdout, output); err != nil {
			return fmt.Errorf("decode script provider %s output: %w", args[0], err)
		}
	}
	return nil
}

func (p Script) Create(ctx context.Context, spec NodeSpec) (Instance, error) {
	var result Instance
	err := p.run(ctx, []string{"create"}, spec, &result)
	return result, err
}
func (p Script) Get(ctx context.Context, id string) (Instance, error) {
	if err := validateID(id); err != nil {
		return Instance{}, err
	}
	instances, err := p.List(ctx, "")
	if err != nil {
		return Instance{}, err
	}
	for _, instance := range instances {
		if instance.ID == id {
			return instance, nil
		}
	}
	return Instance{}, ErrNotFound
}
func (p Script) Delete(ctx context.Context, id string) error {
	if err := validateID(id); err != nil {
		return err
	}
	return p.run(ctx, []string{"delete", id}, nil, nil)
}
func (p Script) List(ctx context.Context, tag string) ([]Instance, error) {
	var instances []Instance
	err := p.run(ctx, []string{"list", tag}, nil, &instances)
	if instances == nil && err == nil {
		instances = []Instance{}
	}
	return instances, err
}
func (p Script) CreateImage(ctx context.Context, instanceID, name string) (string, error) {
	var out struct {
		ID string `json:"id"`
	}
	err := p.run(ctx, []string{"image-create", instanceID, name}, nil, &out)
	return out.ID, err
}
func (p Script) DeleteImage(ctx context.Context, id string) error {
	return p.run(ctx, []string{"image-delete", id}, nil, nil)
}
func (p Script) ListImagesByName(ctx context.Context, name string) ([]string, error) {
	var out struct {
		IDs []string `json:"ids"`
	}
	err := p.run(ctx, []string{"image-list", name}, nil, &out)
	return out.IDs, err
}
func (p Script) ImageStatus(context.Context, string) (string, error) { return "available", nil }

// Fake is an in-memory provider for deterministic autoscaling tests.
type Fake struct {
	mu        sync.Mutex
	instances map[string]Instance
	tags      map[string][]string
	nextID    int
	images    map[string]string
	nextImage int
}

func NewFake() *Fake {
	return &Fake{instances: make(map[string]Instance), tags: make(map[string][]string)}
}

func (p *Fake) Create(ctx context.Context, spec NodeSpec) (Instance, error) {
	if err := ctx.Err(); err != nil {
		return Instance{}, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.instances == nil {
		p.instances = make(map[string]Instance)
		p.tags = make(map[string][]string)
	}
	p.nextID++
	instance := Instance{ID: fmt.Sprintf("fake-%d", p.nextID), Name: spec.Name, PublicIPv4: "192.0.2.1", Status: "active", CreatedAt: time.Now().UTC()}
	p.instances[instance.ID] = instance
	p.tags[instance.ID] = append([]string(nil), spec.Tags...)
	return instance, nil
}

func (p *Fake) Get(ctx context.Context, id string) (Instance, error) {
	if err := ctx.Err(); err != nil {
		return Instance{}, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	instance, ok := p.instances[id]
	if !ok {
		return Instance{}, ErrNotFound
	}
	return instance, nil
}

func (p *Fake) Delete(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.instances[id]; !ok {
		return ErrNotFound
	}
	delete(p.instances, id)
	delete(p.tags, id)
	return nil
}

func (p *Fake) List(ctx context.Context, tag string) ([]Instance, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	instances := make([]Instance, 0)
	for id, instance := range p.instances {
		if tag == "" || contains(p.tags[id], tag) {
			instances = append(instances, instance)
		}
	}
	return instances, nil
}

func (p *Fake) CreateImage(ctx context.Context, id, name string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.images == nil {
		p.images = make(map[string]string)
	}
	p.nextImage++
	imageID := fmt.Sprintf("image-%d", p.nextImage)
	p.images[imageID] = name
	return imageID, nil
}
func (p *Fake) ListImagesByName(ctx context.Context, name string) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	var ids []string
	for id, imageName := range p.images {
		if imageName == name {
			ids = append(ids, id)
		}
	}
	return ids, nil
}
func (p *Fake) DeleteImage(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.images, id)
	return nil
}
func (p *Fake) ImageStatus(context.Context, string) (string, error) { return "available", nil }

func contains(values []string, value string) bool {
	for _, item := range values {
		if item == value {
			return true
		}
	}
	return false
}
