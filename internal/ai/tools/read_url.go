package tools

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"unicode/utf8"

	"github.com/example-git/haunt-server/internal/publichttp"
	"golang.org/x/net/html"
	"golang.org/x/net/html/charset"
)

const maxInputBytes = 2 << 20
const maxMarkdownBytes = 4 << 20
const browserUA = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/157.0.0.0 Safari/537.36"

type readParams struct {
	URL      string `json:"url"`
	Offset   int    `json:"offset"`
	MaxChars *int   `json:"max_chars"`
}
type page struct {
	URL        string `json:"url"`
	Content    string `json:"content"`
	Offset     int    `json:"offset"`
	NextOffset *int   `json:"next_offset,omitempty"`
	TotalChars int    `json:"total_chars"`
}

func (r *Registry) fetch(ctx context.Context, address string) ([]byte, *url.URL, string, error) {
	u, err := publichttp.ParseURL(address)
	if err != nil {
		return nil, nil, "", errors.New("URL must use public HTTP(S), without credentials or a nonstandard port")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, nil, "", err
	}
	req.Header.Set("User-Agent", browserUA)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,text/plain,application/json;q=0.8")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	resp, err := r.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, nil, "", ctx.Err()
		}
		return nil, nil, "", errors.New("public page is unavailable or its network destination is blocked")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		if resp.StatusCode == 202 || resp.StatusCode == 429 {
			return nil, nil, "", errors.New("source blocked or rate limited the request")
		}
		return nil, nil, "", fmt.Errorf("source returned HTTP %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxInputBytes+1))
	if err != nil {
		return nil, nil, "", errors.New("could not read source page")
	}
	if len(b) > maxInputBytes {
		return nil, nil, "", errors.New("source page exceeds 2 MiB input limit")
	}
	kind := resp.Header.Get("Content-Type")
	if kind == "" {
		kind = http.DetectContentType(b)
	}
	mediaType, _, err := mime.ParseMediaType(kind)
	if err != nil {
		return nil, nil, "", errors.New("source has invalid content type")
	}
	switch mediaType {
	case "text/html", "application/xhtml+xml":
		reader, err := charset.NewReader(bytes.NewReader(b), kind)
		if err != nil {
			return nil, nil, "", errors.New("unsupported source charset")
		}
		b, err = io.ReadAll(io.LimitReader(reader, maxInputBytes+1))
		if err != nil || len(b) > maxInputBytes {
			return nil, nil, "", errors.New("decoded source exceeds input limit")
		}
	case "text/plain", "text/markdown", "application/json", "text/json", "application/xml", "text/xml":
	default:
		return nil, nil, "", errors.New("source is not a readable text or HTML document")
	}
	if !utf8.Valid(b) {
		return nil, nil, "", errors.New("source text is not UTF-8")
	}
	return b, resp.Request.URL, mediaType, nil
}
func (r *Registry) readURL(ctx context.Context, p readParams) (page, error) {
	limit := 32768
	if p.MaxChars != nil {
		limit = *p.MaxChars
	}
	if p.Offset < 0 || limit < 1024 || limit > 65536 {
		return page{}, errors.New("offset must be nonnegative and max_chars must be between 1024 and 65536")
	}
	b, u, kind, err := r.fetch(ctx, p.URL)
	if err != nil {
		return page{}, err
	}
	content := string(b)
	if kind == "text/html" || kind == "application/xhtml+xml" {
		content, err = markdown(ctx, b, u)
		if err != nil {
			return page{}, err
		}
	}
	characters := []rune(content)
	if p.Offset > len(characters) {
		return page{}, errors.New("offset exceeds source content length")
	}
	end := min(p.Offset+limit, len(characters))
	out := page{URL: u.String(), Content: string(characters[p.Offset:end]), Offset: p.Offset, TotalChars: len(characters)}
	if end < len(characters) {
		out.NextOffset = &end
	}
	return out, nil
}
func attr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}
func text(n *html.Node) string {
	var out strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.TextNode {
			out.WriteString(n.Data)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return strings.Join(strings.Fields(out.String()), " ")
}
func markdown(ctx context.Context, b []byte, base *url.URL) (string, error) {
	doc, err := html.ParseWithOptions(bytes.NewReader(b), html.ParseOptionEnableScripting(false))
	if err != nil {
		return "", errors.New("invalid HTML")
	}
	var out strings.Builder
	var walk func(*html.Node, bool) error
	walk = func(n *html.Node, pre bool) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if out.Len() > maxMarkdownBytes {
			return errors.New("converted page exceeds output limit")
		}
		if n.Type == html.TextNode {
			if pre {
				out.WriteString(n.Data)
			} else {
				value := strings.Join(strings.Fields(n.Data), " ")
				if strings.TrimSpace(n.Data) != "" {
					if len(n.Data) > 0 && (n.Data[0] == ' ' || n.Data[0] == '\n' || n.Data[0] == '\t') {
						out.WriteByte(' ')
					}
					out.WriteString(value)
					if len(n.Data) > 0 && (n.Data[len(n.Data)-1] == ' ' || n.Data[len(n.Data)-1] == '\n' || n.Data[len(n.Data)-1] == '\t') {
						out.WriteByte(' ')
					}
				}
			}
			return nil
		}
		if n.Type == html.ElementNode {
			switch n.Data {
			case "script", "style", "template", "iframe", "object", "embed", "noscript", "head":
				return nil
			}
			for _, a := range n.Attr {
				if a.Key == "hidden" || (a.Key == "aria-hidden" && a.Val == "true") {
					return nil
				}
			}
			switch n.Data {
			case "h1", "h2", "h3", "h4", "h5", "h6":
				out.WriteString("\n\n" + strings.Repeat("#", int(n.Data[1]-'0')) + " ")
			case "p", "div", "section", "article", "header", "footer", "main", "table", "tr", "ul", "ol":
				out.WriteString("\n\n")
			case "li":
				out.WriteString("\n- ")
			case "br":
				out.WriteByte('\n')
			case "hr":
				out.WriteString("\n\n---\n\n")
			case "strong", "b":
				out.WriteString("**")
			case "em", "i":
				out.WriteByte('*')
			case "blockquote":
				out.WriteString("\n> ")
			case "pre":
				out.WriteString("\n\n```\n")
				pre = true
			case "code":
				if !pre {
					out.WriteByte('`')
				}
			case "a":
				out.WriteByte('[')
			case "img":
				out.WriteString(attr(n, "alt"))
				return nil
			case "td", "th":
				out.WriteString(" | ")
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			if err := walk(c, pre); err != nil {
				return err
			}
		}
		if n.Type == html.ElementNode {
			switch n.Data {
			case "h1", "h2", "h3", "h4", "h5", "h6", "p", "div", "section", "article", "li", "tr":
				out.WriteByte('\n')
			case "strong", "b":
				out.WriteString("**")
			case "em", "i":
				out.WriteByte('*')
			case "pre":
				out.WriteString("\n```\n")
			case "code":
				if !pre {
					out.WriteByte('`')
				}
			case "a":
				target, err := base.Parse(attr(n, "href"))
				if err == nil && target.User == nil && (target.Scheme == "http" || target.Scheme == "https") {
					out.WriteString("](" + strings.NewReplacer("(", "%28", ")", "%29").Replace(target.String()) + ")")
				} else {
					out.WriteByte(']')
				}
			}
		}
		return nil
	}
	if err = walk(doc, false); err != nil {
		return "", err
	}
	lines := strings.Split(out.String(), "\n")
	var clean strings.Builder
	blank := 0
	for _, line := range lines {
		line = strings.TrimRight(line, " \t")
		if line == "" {
			blank++
			if blank > 2 {
				continue
			}
		} else {
			blank = 0
		}
		clean.WriteString(line)
		clean.WriteByte('\n')
	}
	return strings.TrimSpace(clean.String()), nil
}
