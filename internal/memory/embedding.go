package memory

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

const dimensions = 1024

// Embedder speaks the JSON embeddings endpoint of an OpenAI-compatible
// provider. Only endpoints and models that return 1024 dimensions are used.
type Embedder struct {
	BaseURL string
	Model   string
	APIKey  string
}

func (e Embedder) vector(ctx context.Context, input string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	body, err := json.Marshal(map[string]string{"model": e.Model, "input": input})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(e.BaseURL, "/")+"/embeddings", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if e.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+e.APIKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("embedding endpoint returned status %d", resp.StatusCode)
	}
	var result struct {
		Data []struct {
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 256<<10)).Decode(&result); err != nil {
		return "", err
	}
	if len(result.Data) != 1 || len(result.Data[0].Embedding) != dimensions {
		return "", fmt.Errorf("embedding endpoint must return one %d-dimensional vector", dimensions)
	}
	for _, n := range result.Data[0].Embedding {
		if n != 0 {
			vector, err := json.Marshal(result.Data[0].Embedding)
			return string(vector), err
		}
	}
	return "", fmt.Errorf("embedding endpoint returned a zero vector")
}

func (s Store) embedOne(ctx context.Context) error {
	var id, content string
	err := s.DB.QueryRow(ctx, `WITH due AS (
		SELECT id FROM memories
		WHERE deleted_at IS NULL AND embedding_model IS DISTINCT FROM $1
			AND (embedding_next IS NULL OR embedding_next <= now())
		ORDER BY created_at, id LIMIT 1 FOR UPDATE SKIP LOCKED
	)
	UPDATE memories m SET embedding_next = now() + interval '1 minute'
	FROM due WHERE m.id = due.id RETURNING m.id, m.content`, s.Embedder.Model).Scan(&id, &content)
	if err == pgx.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	vector, err := s.Embedder.vector(ctx, content)
	if err != nil {
		return err
	}
	_, err = s.DB.Exec(ctx, `UPDATE memories SET embedding = $2::text::vector, embedding_model = $3, embedding_next = NULL
		WHERE id = $1 AND deleted_at IS NULL`, id, vector, s.Embedder.Model)
	return err
}
