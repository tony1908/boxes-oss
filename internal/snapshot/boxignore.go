package snapshot

import (
	"fmt"
	"path"
	"regexp"
	"strings"
)

type ignoreRule struct {
	base     string
	pattern  *regexp.Regexp
	negated  bool
	dirOnly  bool
	basename bool
}

// Ignored reports whether path is excluded by .boxignore content located at
// ignorePath. Rules are relative to the directory containing ignorePath.
func Ignored(content, ignorePath, candidate string) (bool, error) {
	rules, err := parseIgnoreRules(content, ignorePath)
	if err != nil {
		return false, err
	}
	return ignoredByRules(rules, candidate), nil
}

func parseIgnoreRules(content, ignorePath string) ([]ignoreRule, error) {
	base := path.Dir(path.Clean("/" + strings.TrimPrefix(ignorePath, "/")))
	var rules []ignoreRule
	for lineNumber, line := range strings.Split(content, "\n") {
		line = strings.TrimSuffix(line, "\r")
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		negated := strings.HasPrefix(line, "!")
		if negated {
			line = strings.TrimPrefix(line, "!")
		}
		if line == "" {
			continue
		}
		anchored := strings.HasPrefix(line, "/")
		line = strings.TrimPrefix(line, "/")
		dirOnly := strings.HasSuffix(line, "/")
		line = strings.TrimSuffix(line, "/")
		if line == "" {
			continue
		}
		compiled, err := compileGlob(line)
		if err != nil {
			return nil, fmt.Errorf("parse .boxignore line %d: %w", lineNumber+1, err)
		}
		rules = append(rules, ignoreRule{
			base:     base,
			pattern:  compiled,
			negated:  negated,
			dirOnly:  dirOnly,
			basename: !anchored && !strings.Contains(line, "/"),
		})
	}
	return rules, nil
}

func ignoredByRules(rules []ignoreRule, candidate string) bool {
	ignored, _ := evaluateIgnoreRules(rules, candidate, false)
	return ignored
}

func evaluateIgnoreRules(rules []ignoreRule, candidate string, initial bool) (bool, bool) {
	candidate = path.Clean("/" + strings.TrimPrefix(candidate, "/"))
	ignored := initial
	matchedAny := false
	for _, rule := range rules {
		if rule.base != "/" && candidate != rule.base && !strings.HasPrefix(candidate, rule.base+"/") {
			continue
		}
		relative := strings.TrimPrefix(strings.TrimPrefix(candidate, rule.base), "/")
		if relative == "" {
			continue
		}
		matched := false
		if rule.basename {
			parts := strings.Split(relative, "/")
			for index, part := range parts {
				if rule.pattern.MatchString(part) && (!rule.dirOnly || index < len(parts)-1 || len(parts) == 1) {
					matched = true
					break
				}
			}
		} else if rule.dirOnly {
			parts := strings.Split(relative, "/")
			for index := range parts {
				if rule.pattern.MatchString(strings.Join(parts[:index+1], "/")) {
					matched = true
					break
				}
			}
		} else {
			parts := strings.Split(relative, "/")
			for index := range parts {
				if rule.pattern.MatchString(strings.Join(parts[:index+1], "/")) {
					matched = true
					break
				}
			}
		}
		if matched {
			ignored = !rule.negated
			matchedAny = true
		}
	}
	return ignored, matchedAny
}

func compileGlob(pattern string) (*regexp.Regexp, error) {
	var expression strings.Builder
	expression.WriteByte('^')
	for index := 0; index < len(pattern); index++ {
		character := pattern[index]
		switch character {
		case '*':
			if index+1 < len(pattern) && pattern[index+1] == '*' {
				index++
				if index+1 < len(pattern) && pattern[index+1] == '/' {
					index++
					expression.WriteString("(?:.*/)?")
				} else {
					expression.WriteString(".*")
				}
			} else {
				expression.WriteString("[^/]*")
			}
		case '?':
			expression.WriteString("[^/]")
		case '[':
			end := strings.IndexByte(pattern[index+1:], ']')
			if end < 0 {
				expression.WriteString(`\[`)
				continue
			}
			end += index + 1
			class := pattern[index+1 : end]
			if strings.HasPrefix(class, "!") {
				class = "^" + class[1:]
			}
			expression.WriteByte('[')
			expression.WriteString(class)
			expression.WriteByte(']')
			index = end
		case '\\':
			if index+1 < len(pattern) {
				index++
				expression.WriteString(regexp.QuoteMeta(pattern[index : index+1]))
			} else {
				expression.WriteString(`\\`)
			}
		default:
			expression.WriteString(regexp.QuoteMeta(pattern[index : index+1]))
		}
	}
	expression.WriteByte('$')
	return regexp.Compile(expression.String())
}
