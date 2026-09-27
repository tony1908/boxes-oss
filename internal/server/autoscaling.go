package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/boxes-oss/boxes/internal/diagnostics"
	"github.com/boxes-oss/boxes/internal/provider"
	"github.com/boxes-oss/boxes/internal/store"
	"github.com/boxes-oss/boxes/internal/types"
)

const queuedProvisionTimeout = 4 * time.Minute

func poolTag(id string) string { return "boxes-pool-" + strings.ReplaceAll(id, "_", "-") }

func (s *Server) bakePoolImage(ctx context.Context, record store.PoolRecord) error {
	cloud, err := s.poolProvider(record)
	if err != nil {
		return err
	}
	tag := "boxes-bake"
	publicURL := strings.TrimRight(s.cfg.PublicURL, "/")
	images := s.defaultImages()
	reportToken, err := randomToken(32)
	if err != nil {
		return err
	}
	s.bakeReportMu.Lock()
	if s.bakeReportTokens == nil {
		s.bakeReportTokens = make(map[string]string)
	}
	s.bakeReportTokens[record.Pool.ID] = tokenHash(reportToken)
	s.bakeReportMu.Unlock()
	registry := s.registryConfig()
	userData := bakePoolUserData(publicURL, record.Pool.ID, reportToken, images, registry)
	// Make sure a failed or timed out bake cannot leave a reusable report token.
	defer func() { s.bakeReportMu.Lock(); delete(s.bakeReportTokens, record.Pool.ID); s.bakeReportMu.Unlock() }()
	instance, err := cloud.Create(ctx, provider.NodeSpec{Name: record.Pool.Name + "-bake", Region: record.Pool.Region, Size: record.Pool.Size, Image: record.Pool.Image, UserData: userData, Tags: []string{tag}, SSHKeys: record.Pool.SSHKeys})
	if err != nil {
		return err
	}
	defer func() { _ = cloud.Delete(context.Background(), instance.ID) }()
	imageName := record.Pool.Name + "-baked"
	imageActionStarted := false
	bakeSucceeded := false
	defer func() {
		if !imageActionStarted || bakeSucceeded {
			return
		}
		currentID := record.Pool.BakedImageID
		if current, getErr := s.store.GetPool(context.Background(), record.Pool.ID); getErr == nil {
			currentID = current.Pool.BakedImageID
		}
		ids, listErr := cloud.ListImagesByName(context.Background(), imageName)
		if listErr != nil {
			log.Printf("list failed bake images pool=%s: %v", record.Pool.ID, listErr)
			return
		}
		for _, id := range ids {
			if id == currentID {
				continue
			}
			if deleteErr := cloud.DeleteImage(context.Background(), id); deleteErr != nil {
				log.Printf("delete failed bake image pool=%s image=%s: %v", record.Pool.ID, id, deleteErr)
			}
		}
	}()
	deadline := time.NewTimer(30 * time.Minute)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		current, e := cloud.Get(ctx, instance.ID)
		if e != nil {
			return e
		}
		if strings.EqualFold(current.Status, "off") || strings.EqualFold(current.Status, "powered_off") || strings.EqualFold(current.Status, "stopped") {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return errors.New("timed out waiting for bake instance power-off")
		case <-ticker.C:
		}
	}
	imageActionStarted = true
	imageID, err := cloud.CreateImage(ctx, instance.ID, imageName)
	if err != nil {
		return err
	}
	for {
		status, e := cloud.ImageStatus(ctx, imageID)
		if e != nil {
			return e
		}
		if status == "available" || status == "ready" {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Second):
		}
	}
	latest, err := s.store.GetPool(ctx, record.Pool.ID)
	if err != nil {
		_ = cloud.DeleteImage(context.Background(), imageID)
		return err
	}
	old := latest.Pool.BakedImageID
	images = s.defaultImages()
	latest.Pool.BakedImageID = imageID
	// The authenticated bake-report endpoint stored resolved digests before shutdown.
	if latest.Pool.BakedBoxDigest == "" || latest.Pool.BakedToolsDigest == "" {
		_ = cloud.DeleteImage(context.Background(), imageID)
		return errors.New("bake instance did not report resolved image digests")
	}
	latest.Pool.BakeStatus = "ready"
	if err = s.store.UpdatePool(ctx, latest); err != nil {
		_ = cloud.DeleteImage(context.Background(), imageID)
		return err
	}
	if old != "" {
		_ = cloud.DeleteImage(ctx, old)
	}
	bakeSucceeded = true
	return nil
}

func (s *Server) usageSmallSlots(ctx context.Context, node store.NodeRecord, small types.Size) int {
	snap, ok := s.getHealth(node.Node.ID)
	if !ok || s.now().Sub(snap.At) > 45*time.Second || snap.Health.MemoryBytes <= 0 || snap.Health.MemAvailableBytes <= 0 || snap.Health.MemoryPressureSomeAvg10 >= s.cfg.AdmissionPSIMax {
		return 0
	}
	if _, connected := s.hub.Client(node.Node.ID); !connected {
		return 0
	}
	boxes, err := s.store.ListBoxes(ctx, node.Node.ID)
	if err != nil {
		return 0
	}
	var floor int64
	for _, b := range boxes {
		if b.Box.State != types.StateStopped {
			if z, e := types.SizeFor(b.Box.Type); e == nil {
				floor += z.MemoryReservationBytes
			}
		}
	}
	buffer := int64(math.Max(float64(s.cfg.AdmissionBufferMin), float64(snap.Health.MemoryBytes)*s.cfg.AdmissionBufferPercent))
	if floor+small.MemoryReservationBytes > int64(float64(snap.Health.MemoryBytes)*.95) {
		return 0
	}
	available := snap.Health.MemAvailableBytes - pendingBytes(ctx, s, node.Node.ID, snap.At) - buffer
	byMemory := int(available / expectedBox(s, small))
	byFloor := int((int64(float64(snap.Health.MemoryBytes)*.95) - floor) / small.MemoryReservationBytes)
	byCPU := node.Node.CPUs / small.CPUs
	if byMemory < byFloor {
		byFloor = byMemory
	}
	if byCPU < byFloor {
		byFloor = byCPU
	}
	if byFloor < 0 {
		return 0
	}
	return byFloor
}

func bootingSmallSlots(size string, small types.Size, cfg Config) int {
	ram := int64(4 << 30)
	// Common provider size identifiers encode RAM as a terminal or delimited GiB value.
	for i := 0; i < len(size); i++ {
		if size[i] < '0' || size[i] > '9' {
			continue
		}
		j := i
		for j < len(size) && size[j] >= '0' && size[j] <= '9' {
			j++
		}
		if j < len(size) && (size[j] == 'g' || size[j] == 'G') && (j+1 == len(size) || size[j+1] == 'b' || size[j+1] == 'B') {
			if n, err := strconv.ParseInt(size[i:j], 10, 64); err == nil && n > 0 {
				ram = n << 30
			}
		}
		i = j
	}
	buffer := int64(math.Max(float64(cfg.AdmissionBufferMin), float64(ram)*cfg.AdmissionBufferPercent))
	if ram <= buffer {
		return 0
	}
	perBox := int64(300 << 20)
	if cfg.AdmissionExpectedBox > perBox {
		perBox = cfg.AdmissionExpectedBox
	}
	return int((ram - buffer) / perBox)
}

func bakePoolUserData(publicURL, poolID, token string, images defaultImageConfig, registry registryConfig) string {
	publicURL = strings.TrimRight(publicURL, "/")
	command := "curl -fsSL " + shellQuote(publicURL+"/install/node-install.sh") + " | env BOXES_NODE_MODE=bake BOXES_PUBLIC_URL=" + shellQuote(publicURL) + " BOXES_BAKE_REPORT_URL=" + shellQuote(publicURL+"/api/v1/pools/"+poolID+"/bake-report") + " BOXES_BAKE_TOKEN=" + shellQuote(token) + " BOXES_BOX_IMAGE=" + shellQuote(images.Box) + " BOXES_TOOLS_IMAGE=" + shellQuote(images.Tools)
	if registry.URL != "" {
		command += " BOXES_REGISTRY_URL=" + shellQuote(registry.URL) + " BOXES_REGISTRY_USERNAME=" + shellQuote(registry.Username) + " BOXES_REGISTRY_PASSWORD=" + shellQuote(registry.Password)
	}
	command += " bash -s --"
	quoted, _ := json.Marshal(command)
	return "#cloud-config\nruncmd:\n  - [bash, -lc, " + string(quoted) + "]\n"
}

func countedPoolNode(node types.Node) bool {
	return node.Status != "destroyed" && node.Status != "lost"
}

func (s *Server) poolUserData(ctx context.Context, token string, poolOptions ...types.Pool) (string, error) {
	publicURL := strings.TrimRight(s.cfg.PublicURL, "/")
	tunnelURL := publicURL
	switch {
	case strings.HasPrefix(publicURL, "https://"):
		tunnelURL = "wss://" + strings.TrimPrefix(publicURL, "https://")
	case strings.HasPrefix(publicURL, "http://"):
		tunnelURL = "ws://" + strings.TrimPrefix(publicURL, "http://")
	}
	tunnelURL += "/api/v1/agent/connect"
	environment, args := nodeInstallEnvironment(publicURL, tunnelURL, token, s.storageConfig(), s.registryConfig(), s.defaultImages())
	if len(poolOptions) > 0 {
		pool := poolOptions[0]
		if pool.BakedBoxDigest != "" {
			environment["BOXES_BOX_DIGEST"] = pool.BakedBoxDigest
		}
		if pool.BakedToolsDigest != "" {
			environment["BOXES_TOOLS_DIGEST"] = pool.BakedToolsDigest
		}
	}
	keys := make([]string, 0, len(environment))
	for key := range environment {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	command := "curl -fsSL " + shellQuote(publicURL+"/install/node-install.sh") + " | env"
	for _, key := range keys {
		command += " " + key + "=" + shellQuote(environment[key])
	}
	command += " bash -s --"
	for _, arg := range args {
		command += " " + shellQuote(arg)
	}
	quoted, err := json.Marshal(command)
	if err != nil {
		return "", err
	}
	return "#cloud-config\nruncmd:\n  - [bash, -lc, " + string(quoted) + "]\n", nil
}

func (s *Server) createPoolNode(ctx context.Context, poolID string) (store.NodeRecord, error) {
	s.autoscaleMu.Lock()
	defer s.autoscaleMu.Unlock()
	return s.createPoolNodeLocked(ctx, poolID)
}

func (s *Server) createPoolNodeLocked(ctx context.Context, poolID string) (store.NodeRecord, error) {
	pool, err := s.store.GetPool(ctx, poolID)
	if err != nil {
		return store.NodeRecord{}, err
	}
	nodes, err := s.store.ListNodes(ctx)
	if err != nil {
		return store.NodeRecord{}, err
	}
	poolNodes := make([]store.NodeRecord, 0)
	for _, node := range nodes {
		if node.Node.PoolID == poolID && countedPoolNode(node.Node) {
			poolNodes = append(poolNodes, node)
		}
	}
	if len(poolNodes) >= pool.Pool.MaxNodes {
		return store.NodeRecord{}, fmt.Errorf("pool %q is at its maxNodes limit", pool.Pool.Name)
	}
	cloud, err := s.poolProvider(pool)
	if err != nil {
		return store.NodeRecord{}, err
	}
	token, err := randomToken(32)
	if err != nil {
		return store.NodeRecord{}, err
	}
	now := time.Now().UTC()
	ordinal := len(poolNodes) + 1
	name := fmt.Sprintf("%s-%02d", pool.Pool.Name, ordinal)
	node := types.Node{ID: types.NewID("nd_"), Name: name, Status: "provisioning", PoolID: poolID, CreatedAt: now}
	record := store.NodeRecord{Node: node, TokenHash: tokenHash(token)}
	if err := s.store.CreateNode(ctx, record); err != nil {
		return store.NodeRecord{}, err
	}
	userData, err := s.poolUserData(ctx, token, pool.Pool)
	if err != nil {
		_ = s.store.DeleteNode(context.Background(), node.ID)
		return store.NodeRecord{}, err
	}
	image := pool.Pool.Image
	if pool.Pool.BakedImageID != "" {
		image = pool.Pool.BakedImageID
	}
	instance, err := cloud.Create(ctx, provider.NodeSpec{Name: name, Region: pool.Pool.Region, Size: pool.Pool.Size,
		Image: image, UserData: userData, Tags: []string{poolTag(poolID)}, SSHKeys: pool.Pool.SSHKeys})
	if err != nil {
		_ = s.store.DeleteNode(context.Background(), node.ID)
		return store.NodeRecord{}, err
	}
	if instance.ID == "" {
		_ = s.store.DeleteNode(context.Background(), node.ID)
		return store.NodeRecord{}, errors.New("provider returned an empty instance ID")
	}
	record.Node.ProviderInstanceID = instance.ID
	record.Node.Host = instance.PublicIPv4
	if instance.PublicIPv4 != "" {
		record.Node.HostBase = instance.PublicIPv4 + ".sslip.io"
	}
	if err := s.store.UpdateNode(ctx, record); err != nil {
		_ = cloud.Delete(context.Background(), instance.ID)
		_ = s.store.DeleteNode(context.Background(), node.ID)
		return store.NodeRecord{}, err
	}
	return record, nil
}

func (s *Server) choosePoolForScaleUp(ctx context.Context) (types.Pool, error) {
	pools, err := s.store.ListPools(ctx)
	if err != nil {
		return types.Pool{}, err
	}
	nodes, err := s.store.ListNodes(ctx)
	if err != nil {
		return types.Pool{}, err
	}
	type candidate struct {
		pool     types.Pool
		headroom int
	}
	var candidates []candidate
	for _, record := range pools {
		count := 0
		for _, node := range nodes {
			if node.Node.PoolID == record.Pool.ID && countedPoolNode(node.Node) {
				count++
			}
		}
		if count < record.Pool.MaxNodes {
			candidates = append(candidates, candidate{record.Pool, record.Pool.MaxNodes - count})
		}
	}
	if len(candidates) == 0 {
		return types.Pool{}, sql.ErrNoRows
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].headroom == candidates[j].headroom {
			return candidates[i].pool.ID < candidates[j].pool.ID
		}
		return candidates[i].headroom > candidates[j].headroom
	})
	return candidates[0].pool, nil
}

func (s *Server) scaleUpForCapacity(ctx context.Context) (store.NodeRecord, error) {
	s.autoscaleMu.Lock()
	defer s.autoscaleMu.Unlock()
	nodes, err := s.store.ListNodes(ctx)
	if err != nil {
		return store.NodeRecord{}, err
	}
	var pending []store.NodeRecord
	for _, node := range nodes {
		if node.Node.PoolID != "" && node.Node.Status == "provisioning" && node.Node.ProviderInstanceID != "" {
			pending = append(pending, node)
		}
	}
	if len(pending) != 0 {
		sort.Slice(pending, func(i, j int) bool {
			if pending[i].Node.CreatedAt.Equal(pending[j].Node.CreatedAt) {
				return pending[i].Node.ID < pending[j].Node.ID
			}
			return pending[i].Node.CreatedAt.Before(pending[j].Node.CreatedAt)
		})
		return pending[0], nil
	}
	pool, err := s.choosePoolForScaleUp(ctx)
	if err != nil {
		return store.NodeRecord{}, err
	}
	return s.createPoolNodeLocked(ctx, pool.ID)
}

func (s *Server) ensurePoolMinimum(ctx context.Context, poolID string) {
	for ctx.Err() == nil {
		s.autoscaleMu.Lock()
		pool, err := s.store.GetPool(ctx, poolID)
		if err != nil {
			s.autoscaleMu.Unlock()
			return
		}
		nodes, err := s.store.ListNodes(ctx)
		if err != nil {
			s.autoscaleMu.Unlock()
			return
		}
		count := 0
		for _, node := range nodes {
			if node.Node.PoolID == poolID && countedPoolNode(node.Node) {
				count++
			}
		}
		if count >= effectivePoolMinimum(pool.Pool.MinNodes, pool.Pool.WarmSchedule, s.now()) {
			s.autoscaleMu.Unlock()
			return
		}
		_, err = s.createPoolNodeLocked(ctx, poolID)
		s.autoscaleMu.Unlock()
		if err != nil {
			log.Printf("scale pool %s to minNodes failed: %v", poolID, err)
			return
		}
	}
}

// ensurePoolHeadroom adds capacity before a pool runs out of room for small boxes.
func (s *Server) ensurePoolHeadroom(ctx context.Context, poolID string) {
	pool, err := s.store.GetPool(ctx, poolID)
	if err != nil {
		return
	}
	target := pool.Pool.HeadroomSlots
	if target == 0 {
		return
	}
	nodes, err := s.store.ListNodes(ctx)
	if err != nil {
		return
	}
	free := 0
	small, _ := types.SizeFor("small")
	for _, node := range nodes {
		if node.Node.PoolID != poolID {
			continue
		}
		if node.Node.Status == "provisioning" {
			if s.cfg.AdmissionMode == "reservation" {
				free += 4
			} else {
				free += bootingSmallSlots(pool.Pool.Size, small, s.cfg)
			}
			continue
		}
		if node.Node.Status != "healthy" || node.Node.Drained {
			continue
		}
		if s.cfg.AdmissionMode != "reservation" {
			free += s.usageSmallSlots(ctx, node, small)
			continue
		}
		allocated, err := s.allocatedMemory(ctx, node.Node.ID)
		if err != nil {
			continue
		}
		budget := memoryAdmissionBudget(node.Node.MemoryBytes, s.cfg.MemoryOvercommit) - float64(allocated)
		byMemory := int(budget / float64(small.MemoryReservationBytes))
		byCPU := node.Node.CPUs / small.CPUs
		if byCPU < byMemory {
			byMemory = byCPU
		}
		if byMemory > 0 {
			free += byMemory
		}
	}
	if free < target {
		_, _ = s.createPoolNode(ctx, poolID)
	}
}

func scaleDownPreservesHeadroom(ctx context.Context, s *Server, poolID, removingNodeID string, target int) bool {
	nodes, err := s.store.ListNodes(ctx)
	if err != nil {
		return false
	}
	small, _ := types.SizeFor("small")
	free := 0
	for _, node := range nodes {
		if node.Node.PoolID != poolID || node.Node.ID == removingNodeID {
			continue
		}
		if node.Node.Status == "provisioning" {
			if s.cfg.AdmissionMode == "reservation" {
				free += 4
			} else {
				pool, e := s.store.GetPool(ctx, poolID)
				if e == nil {
					free += bootingSmallSlots(pool.Pool.Size, small, s.cfg)
				}
			}
			continue
		}
		if node.Node.Status != "healthy" || node.Node.Drained {
			continue
		}
		if s.cfg.AdmissionMode != "reservation" {
			free += s.usageSmallSlots(ctx, node, small)
			continue
		}
		allocated, err := s.allocatedMemory(ctx, node.Node.ID)
		if err != nil {
			continue
		}
		byMemory := int((memoryAdmissionBudget(node.Node.MemoryBytes, s.cfg.MemoryOvercommit) - float64(allocated)) / float64(small.MemoryReservationBytes))
		byCPU := node.Node.CPUs / small.CPUs
		if byCPU < byMemory {
			byMemory = byCPU
		}
		if byMemory > 0 {
			free += byMemory
		}
	}
	return free >= target
}

// RunAutoscaling starts reconciliation, scale-down, and warm-node maintenance loops.
func (s *Server) RunAutoscaling(ctx context.Context) {
	go s.reconcilePools(ctx)
	go s.scaleDownPools(ctx)
	go func() {
		s.ensureAllPoolMinimums(ctx)
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.ensureAllPoolMinimums(ctx)
			}
		}
	}()
}

func (s *Server) ensureAllPoolMinimums(ctx context.Context) {
	pools, err := s.store.ListPools(ctx)
	if err != nil {
		return
	}
	for _, pool := range pools {
		s.ensurePoolMinimum(ctx, pool.Pool.ID)
		s.ensurePoolHeadroom(ctx, pool.Pool.ID)
	}
}

func (s *Server) scaleDownPools(ctx context.Context) {
	s.scaleDownOnce(ctx)
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.scaleDownOnce(ctx)
		}
	}
}

func (s *Server) scaleDownOnce(ctx context.Context) {
	pools, err := s.store.ListPools(ctx)
	if err != nil {
		return
	}
	poolByID := make(map[string]types.Pool, len(pools))
	for _, pool := range pools {
		poolByID[pool.Pool.ID] = pool.Pool
	}
	nodes, err := s.store.ListNodes(ctx)
	if err != nil {
		return
	}
	counts := make(map[string]int)
	for _, node := range nodes {
		if node.Node.PoolID != "" && countedPoolNode(node.Node) {
			counts[node.Node.PoolID]++
		}
	}
	now := s.now().UTC()
	for _, node := range nodes {
		pool, ok := poolByID[node.Node.PoolID]
		if !ok || node.Node.Status == "destroyed" {
			continue
		}
		boxes, err := s.store.ListBoxes(ctx, node.Node.ID)
		if err != nil {
			continue
		}
		busy := false
		for _, box := range boxes {
			if box.Box.State != types.StateStopped {
				busy = true
				break
			}
		}
		if busy {
			if node.Node.IdleSince != nil {
				node.Node.IdleSince = nil
				_ = s.store.UpdateNode(ctx, node)
			}
			continue
		}
		if node.Node.IdleSince == nil {
			node.Node.IdleSince = &now
			_ = s.store.UpdateNode(ctx, node)
			continue
		}
		if counts[pool.ID] <= effectivePoolMinimum(pool.MinNodes, pool.WarmSchedule, now) || now.Sub(*node.Node.IdleSince) < time.Duration(pool.IdleMinutes)*time.Minute {
			continue
		}
		if pool.HeadroomSlots > 0 && !scaleDownPreservesHeadroom(ctx, s, pool.ID, node.Node.ID, pool.HeadroomSlots) {
			continue
		}
		node.Node.Drained = true
		_ = s.store.UpdateNode(ctx, node)
		record, err := s.store.GetPool(ctx, pool.ID)
		if err != nil {
			continue
		}
		cloud, err := s.poolProvider(record)
		if err != nil {
			continue
		}
		if node.Node.ProviderInstanceID != "" {
			if err := cloud.Delete(ctx, node.Node.ProviderInstanceID); err != nil && !errors.Is(err, provider.ErrNotFound) {
				log.Printf("delete idle node %s failed: %v", node.Node.ID, err)
				continue
			}
		}
		if len(boxes) == 0 {
			_ = s.store.DeleteNode(ctx, node.Node.ID)
		} else {
			node.Node.Status = "destroyed"
			node.Node.PoolID = ""
			node.Node.ProviderInstanceID = ""
			node.Node.Host = ""
			node.Node.HostBase = ""
			node.TokenHash = ""
			_ = s.store.UpdateNode(ctx, node)
		}
		counts[pool.ID]--
	}
	s.expireProvisioningNodes(ctx, nodes)
	s.cleanupDetachedNodes(ctx)
}

func (s *Server) reconcilePools(ctx context.Context) {
	s.reconcilePoolsOnce(ctx)
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.reconcilePoolsOnce(ctx)
		}
	}
}

func (s *Server) reconcilePoolsOnce(ctx context.Context) {
	pools, err := s.store.ListPools(ctx)
	if err != nil {
		return
	}
	nodes, err := s.store.ListNodes(ctx)
	if err != nil {
		return
	}
	for _, pool := range pools {
		cloud, err := s.poolProvider(pool)
		if err != nil {
			log.Printf("reconcile pool %s failed: %v", pool.Pool.ID, err)
			continue
		}
		instances, err := cloud.List(ctx, poolTag(pool.Pool.ID))
		if err != nil {
			log.Printf("list instances for pool %s failed: %v", pool.Pool.ID, err)
			continue
		}
		byID := make(map[string]provider.Instance, len(instances))
		for _, instance := range instances {
			byID[instance.ID] = instance
		}
		known := make(map[string]bool)
		for _, node := range nodes {
			if node.Node.PoolID == pool.Pool.ID && node.Node.ProviderInstanceID != "" {
				known[node.Node.ProviderInstanceID] = true
			}
		}
		for _, instance := range instances {
			if known[instance.ID] {
				continue
			}
			if !instance.CreatedAt.IsZero() && time.Since(instance.CreatedAt) > 30*time.Minute {
				_ = cloud.Delete(ctx, instance.ID)
			}
		}
		for _, node := range nodes {
			if node.Node.PoolID != pool.Pool.ID || node.Node.ProviderInstanceID == "" {
				continue
			}
			if _, exists := byID[node.Node.ProviderInstanceID]; exists {
				continue
			}
			node.Node.Status = "lost"
			node.TokenHash = ""
			_ = s.store.UpdateNode(ctx, node)
			boxes, _ := s.store.ListBoxes(ctx, node.Node.ID)
			for _, box := range boxes {
				box.Box.State = types.StateUnreachable
				if s.store.UpdateBox(ctx, box) == nil {
					s.recordSystemActivity(ctx, box.Box.ID, "autoscaler", "lifecycle", "error", map[string]any{"reason": "node was lost"}, "error")
				}
			}
		}
	}
	s.expireProvisioningNodes(ctx, nodes)
	s.cleanupDetachedNodes(ctx)
}

func (s *Server) expireProvisioningNodes(ctx context.Context, nodes []store.NodeRecord) {
	now := time.Now().UTC()
	for _, node := range nodes {
		if node.Node.PoolID == "" || node.Node.Status != "provisioning" || node.Node.CreatedAt.IsZero() {
			continue
		}
		pool, err := s.store.GetPool(ctx, node.Node.PoolID)
		if err != nil || now.Sub(node.Node.CreatedAt) < time.Duration(pool.Pool.BootTimeoutMinutes)*time.Minute {
			continue
		}
		if node.Node.ProviderInstanceID != "" {
			cloud, err := s.poolProvider(pool)
			if err != nil {
				continue
			}
			if deleteErr := cloud.Delete(ctx, node.Node.ProviderInstanceID); deleteErr != nil && !errors.Is(deleteErr, provider.ErrNotFound) {
				continue
			}
		}
		boxes, _ := s.store.ListBoxes(ctx, node.Node.ID)
		for _, box := range boxes {
			box.Box.State = types.StateError
			box.Box.ErrorCode = "no_capacity"
			box.Box.Error = "No pool node became available before the boot timeout."
			_ = s.store.UpdateBox(ctx, box)
			_ = s.store.DeletePendingBox(ctx, box.Box.ID)
		}
		node.Node.Status = "destroyed"
		node.Node.PoolID = ""
		node.Node.ProviderInstanceID = ""
		node.Node.Host = ""
		node.Node.HostBase = ""
		node.TokenHash = ""
		_ = s.store.UpdateNode(ctx, node)
	}
}

func (s *Server) provisionPendingBoxes(ctx context.Context, nodeID string, client *agentClient) {
	// Pending work belongs to the server lifecycle, not the short-lived request
	// or connect callback that may have discovered the node.
	ctx = s.lifecycleCtx
	s.pendingMu.Lock()
	defer s.pendingMu.Unlock()
	queued, err := s.store.ListPendingBoxes(ctx, nodeID)
	if err != nil {
		return
	}
	for boxID, encrypted := range queued {
		box, boxErr := s.store.GetBox(ctx, boxID)
		if boxErr != nil {
			continue
		}
		size, sizeErr := types.SizeFor(box.Box.Type)
		if sizeErr != nil {
			box.Box.State = types.StateError
			box.Box.ErrorCode = "invalid_type"
			box.Box.Error = "The requested box type is invalid."
			_ = s.store.UpdateBox(ctx, box)
			s.recordSystemActivity(ctx, boxID, "system", "lifecycle", "error", map[string]any{"reason": sizeErr.Error()}, "error")
			_ = s.store.DeletePendingBox(ctx, boxID)
			continue
		}
		if _, capacityErr := s.scheduleExcluding(ctx, nodeID, size, box.Box.Type, boxID); capacityErr != nil {
			var noCapacity *capacityError
			if !errors.As(capacityErr, &noCapacity) {
				continue
			}
			replacement, scaleErr := s.scaleUpForCapacity(ctx)
			if scaleErr != nil {
				box.Box.State = types.StateError
				box.Box.WaitingFor = ""
				box.Box.ErrorCode = "no_capacity"
				box.Box.Error = "No pool node has capacity for this box."
				_ = s.store.UpdateBox(ctx, box)
				s.recordSystemActivity(ctx, boxID, "system", "lifecycle", "error", map[string]any{"reason": "No pool node has capacity for this box."}, "error")
				_ = s.store.DeletePendingBox(ctx, boxID)
				continue
			}
			oldNodeID := box.Box.NodeID
			box.Box.NodeID = replacement.Node.ID
			box.Box.WaitingFor = "node"
			if s.store.UpdateBox(ctx, box) != nil {
				continue
			}
			if oldNodeID != replacement.Node.ID {
				s.recordSystemActivity(ctx, boxID, "autoscaler", "lifecycle", "moved to node", map[string]any{"oldNodeId": oldNodeID, "node": replacement.Node.ID}, "ok")
			}
			continue
		}
		plain, err := s.secrets.decrypt(encrypted)
		if err != nil {
			log.Printf("decrypt queued sandbox %s failed", boxID)
			continue
		}
		var payload map[string]any
		if err := json.Unmarshal(plain, &payload); err != nil {
			log.Printf("decode queued sandbox %s failed", boxID)
			continue
		}
		provisionCtx, cancel := context.WithTimeout(ctx, queuedProvisionTimeout)
		err = client.json(provisionCtx, http.MethodPost, "/v1/boxes", payload, nil)
		cancel()
		if err != nil {
			log.Printf("provision queued sandbox %s failed: %v", boxID, err)
			if box, getErr := s.store.GetBox(ctx, boxID); getErr == nil {
				box.Box.State = types.StateError
				box.Box.ErrorCode = "provision_failed"
				box.Box.Error = diagnostics.Sanitize(err.Error())
				_ = s.store.UpdateBox(ctx, box)
				s.recordSystemActivity(ctx, boxID, "system", "lifecycle", "error", map[string]any{"reason": err.Error()}, "error")
			}
			_ = s.store.DeletePendingBox(ctx, boxID)
			continue
		}
		box, err = s.store.GetBox(ctx, boxID)
		if err == nil {
			box.Box.State = types.StateReady
			now := time.Now().UTC()
			box.RunningSince = &now
			box.Box.WaitingFor = ""
			box.Box.ErrorCode = ""
			box.Box.Error = ""
			if s.store.UpdateBox(ctx, box) == nil {
				_ = s.store.OpenRunningInterval(ctx, boxID, now)
				s.recordSystemActivity(ctx, boxID, "system", "lifecycle", "started/ready", map[string]any{"node": box.Box.NodeID}, "ok")
				s.syncEventPump(box.Box)
				event := "sandbox.ready"
				if box.Box.FromSnapshot != "" {
					event = "sandbox.restored"
				}
				s.emitWebhookEvent(context.Background(), event, map[string]any{"sandbox": box.Box})
			}
		}
		_ = s.store.DeletePendingBox(ctx, boxID)
	}
}
