package extension

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"time"

	sqlc "github.com/taills/moduless/core/db/sqlc"
	"github.com/taills/moduless/core/tunnel"
	pb "github.com/taills/moduless/proto/tunnel"
)

// ErrNotFound is returned when an admin operation targets an unknown extension.
var ErrNotFound = errors.New("extension not found")

// Coordinator drives the admin-facing approval workflow, bridging the registry
// store, the live tunnel manager, and the schema/slot provisioning hooks.
type Coordinator struct {
	Store   *Store
	Manager *tunnel.TunnelManager

	// Provision runs at activation to reconcile CMDS schema and register UI slots
	// from a manifest; it mirrors the tunnel server's OnRegister hook.
	Provision func(*pb.RegisterRequest) error
	// OnUnregister drops key-scoped state (UI slots) when an extension is taken
	// fully offline by a reject/delete.
	OnUnregister func(key string)
}

// IssuedSecret reports a secret minted for one approved instance.
type IssuedSecret struct {
	InstanceID string `json:"instance_id"`
	Secret     string `json:"secret"`
}

// Approve marks an extension approved and activates every tunnel currently
// parked as pending: each instance gets its own freshly-minted secret pushed
// over the tunnel (which the SDK persists), is provisioned, and becomes routable.
func (c *Coordinator) Approve(ctx context.Context, key string) ([]IssuedSecret, error) {
	if _, err := c.Store.q.GetExtension(ctx, key); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if err := c.Store.q.SetExtensionStatus(ctx, sqlc.SetExtensionStatusParams{Key: key, Status: StatusApproved}); err != nil {
		return nil, fmt.Errorf("approve %s: %w", key, err)
	}

	pendings := c.Manager.TakePending(key)
	issued := make([]IssuedSecret, 0, len(pendings))
	for _, t := range pendings {
		secret, err := c.Store.mintSecret(ctx, key, t.InstanceID)
		if err != nil {
			return issued, err
		}
		if c.Provision != nil && t.Meta != nil {
			if err := c.Provision(t.Meta); err != nil {
				log.Printf("[extension] provision on approve failed for %s: %v", key, err)
				_ = t.Send(decisionMsg(&pb.RegisterDecision{Status: "rejected"}))
				continue
			}
		}
		c.Manager.Adopt(t)
		t.Approved.Store(true)

		uploadFE := t.Meta != nil && !t.Meta.IsDev && t.Meta.ZipFileSize > 0
		_ = t.Send(decisionMsg(&pb.RegisterDecision{
			Status:         "approved",
			IssuedSecret:   secret,
			UploadFrontend: uploadFE,
		}))
		if !uploadFE {
			_ = t.Send(responseMsg(&pb.RegisterResponse{Success: true, SkipUpload: true}))
		}
		issued = append(issued, IssuedSecret{InstanceID: t.InstanceID, Secret: secret})
	}
	log.Printf("[extension] approved %s, activated %d instance(s)", key, len(issued))
	return issued, nil
}

// Reject marks an extension rejected, revokes all its secrets, and forces every
// live tunnel (pending or routable) offline.
func (c *Coordinator) Reject(ctx context.Context, key string) error {
	if _, err := c.Store.q.GetExtension(ctx, key); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	if err := c.Store.q.SetExtensionStatus(ctx, sqlc.SetExtensionStatusParams{Key: key, Status: StatusRejected}); err != nil {
		return fmt.Errorf("reject %s: %w", key, err)
	}
	c.revokeAll(ctx, key)
	c.disconnectAll(key)
	return nil
}

// Delete removes an extension and all its secrets, forcing it offline. The next
// time the extension dials Core it is treated as a fresh pending request.
func (c *Coordinator) Delete(ctx context.Context, key string) error {
	c.disconnectAll(key)
	if err := c.Store.q.DeleteExtension(ctx, key); err != nil {
		return fmt.Errorf("delete %s: %w", key, err)
	}
	return nil
}

// GenerateSecret mints an additional secret for an approved extension so an
// operator can bake it into another replica. The plaintext is returned once.
func (c *Coordinator) GenerateSecret(ctx context.Context, key, label string) (string, error) {
	ext, err := c.Store.q.GetExtension(ctx, key)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", ErrNotFound
		}
		return "", err
	}
	if ext.Status != StatusApproved {
		return "", errors.New("extension must be approved before issuing secrets")
	}
	return c.Store.mintSecret(ctx, key, label)
}

// RevokeSecret revokes a single secret of an extension.
func (c *Coordinator) RevokeSecret(ctx context.Context, key string, id int64) error {
	return c.Store.q.RevokeExtensionSecret(ctx, sqlc.RevokeExtensionSecretParams{ID: id, ExtensionKey: key})
}

func (c *Coordinator) revokeAll(ctx context.Context, key string) {
	secrets, err := c.Store.q.ListActiveExtensionSecrets(ctx, key)
	if err != nil {
		log.Printf("[extension] list secrets for revoke %s: %v", key, err)
		return
	}
	for _, s := range secrets {
		_ = c.Store.q.RevokeExtensionSecret(ctx, sqlc.RevokeExtensionSecretParams{ID: s.ID, ExtensionKey: key})
	}
}

// disconnectAll tells every live tunnel for key to stop and drops it from the
// manager, then runs OnUnregister to clear key-scoped state.
func (c *Coordinator) disconnectAll(key string) {
	for _, t := range c.Manager.TakePending(key) {
		_ = t.Send(decisionMsg(&pb.RegisterDecision{Status: "rejected"}))
	}
	for _, t := range c.Manager.RemoveAllForKey(key) {
		_ = t.Send(decisionMsg(&pb.RegisterDecision{Status: "rejected"}))
	}
	if c.OnUnregister != nil {
		c.OnUnregister(key)
	}
}

// ExtensionView is one row in the admin extension list: the registry record plus
// live runtime status.
type ExtensionView struct {
	Key              string     `json:"key"`
	DisplayName      string     `json:"display_name"`
	Version          string     `json:"version"`
	MenuIcon         string     `json:"menu_icon"`
	MenuPath         string     `json:"menu_path"`
	Status           string     `json:"status"`
	Online           bool       `json:"online"`
	Replicas         int        `json:"replicas"`
	PendingInstances int        `json:"pending_instances"`
	CreatedAt        *time.Time `json:"created_at,omitempty"`
	ApprovedAt       *time.Time `json:"approved_at,omitempty"`
}

// List returns every registered extension merged with live tunnel status.
func (c *Coordinator) List(ctx context.Context) ([]ExtensionView, error) {
	rows, err := c.Store.q.ListExtensions(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]ExtensionView, 0, len(rows))
	for _, e := range rows {
		replicas := c.Manager.CountReplicas(e.Key)
		out = append(out, ExtensionView{
			Key:              e.Key,
			DisplayName:      e.DisplayName,
			Version:          e.Version,
			MenuIcon:         e.MenuIcon,
			MenuPath:         e.MenuPath,
			Status:           e.Status,
			Online:           replicas > 0,
			Replicas:         replicas,
			PendingInstances: c.Manager.CountPending(e.Key),
			CreatedAt:        nullTime(e.CreatedAt),
			ApprovedAt:       nullTime(e.ApprovedAt),
		})
	}
	return out, nil
}

// SecretView is one row in the per-extension secret list (no plaintext).
type SecretView struct {
	ID         int64      `json:"id"`
	Label      string     `json:"label"`
	CreatedAt  *time.Time `json:"created_at,omitempty"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
}

// ListSecrets returns all secrets (active and revoked) for an extension.
func (c *Coordinator) ListSecrets(ctx context.Context, key string) ([]SecretView, error) {
	rows, err := c.Store.q.ListExtensionSecrets(ctx, key)
	if err != nil {
		return nil, err
	}
	out := make([]SecretView, 0, len(rows))
	for _, r := range rows {
		out = append(out, SecretView{
			ID:         r.ID,
			Label:      r.Label,
			CreatedAt:  nullTime(r.CreatedAt),
			LastUsedAt: nullTime(r.LastUsedAt),
			RevokedAt:  nullTime(r.RevokedAt),
		})
	}
	return out, nil
}

func nullTime(t sql.NullTime) *time.Time {
	if !t.Valid {
		return nil
	}
	return &t.Time
}

func decisionMsg(d *pb.RegisterDecision) *pb.TunnelMessage {
	return &pb.TunnelMessage{Payload: &pb.TunnelMessage_RegisterDecision{RegisterDecision: d}}
}

func responseMsg(r *pb.RegisterResponse) *pb.TunnelMessage {
	return &pb.TunnelMessage{Payload: &pb.TunnelMessage_RegisterResp{RegisterResp: r}}
}
