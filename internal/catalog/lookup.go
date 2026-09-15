package catalog

import "sort"

// byKey indexes the catalog for O(1) lookup. Built once at init and never
// mutated, so it is safe to read from any goroutine.
var byKey = func() map[string]Sport {
	index := make(map[string]Sport, len(sports))
	for _, sport := range sports {
		if _, clash := index[sport.Key]; clash {
			panic("catalog: duplicate sport key " + sport.Key)
		}
		index[sport.Key] = sport
	}
	return index
}()

// Lookup returns the sport with the given key.
func Lookup(key string) (Sport, bool) {
	sport, ok := byKey[key]
	return sport, ok
}

// All returns every sport, sorted by name.
func All() []Sport {
	out := make([]Sport, len(sports))
	copy(out, sports)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Count is the number of sports the book offers.
func Count() int { return len(sports) }

// CategoryGroup is one heading in the sport menu with its sports beneath it.
type CategoryGroup struct {
	Category Category
	Sports   []Sport
}

// Grouped returns the sports arranged into their categories, in menu order.
func Grouped() []CategoryGroup {
	buckets := make(map[Category][]Sport, len(categoryOrder))
	for _, sport := range sports {
		buckets[sport.Category] = append(buckets[sport.Category], sport)
	}
	groups := make([]CategoryGroup, 0, len(categoryOrder))
	for _, category := range categoryOrder {
		members := buckets[category]
		if len(members) == 0 {
			continue
		}
		sort.Slice(members, func(i, j int) bool { return members[i].Name < members[j].Name })
		groups = append(groups, CategoryGroup{Category: category, Sports: members})
	}
	return groups
}

// Categories returns the category headings in menu order.
func Categories() []Category {
	out := make([]Category, len(categoryOrder))
	copy(out, categoryOrder)
	return out
}

// InCategory returns the sports in one category, sorted by name.
func InCategory(category Category) []Sport {
	var out []Sport
	for _, sport := range sports {
		if sport.Category == category {
			out = append(out, sport)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Search returns sports whose name or competition list matches the query.
// The match is a case-insensitive substring: the sport menu is small enough
// that anything cleverer would be a liability rather than a feature.
func Search(query string) []Sport {
	query = foldSpace(query)
	if query == "" {
		return nil
	}
	var out []Sport
	for _, sport := range sports {
		if contains(foldSpace(sport.Name), query) || contains(foldSpace(string(sport.Category)), query) {
			out = append(out, sport)
			continue
		}
		for _, competition := range sport.Competitions {
			if contains(foldSpace(competition), query) {
				out = append(out, sport)
				break
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func foldSpace(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		if r >= 'A' && r <= 'Z' {
			r += 'a' - 'A'
		}
		out = append(out, r)
	}
	return string(out)
}

func contains(haystack, needle string) bool {
	if len(needle) > len(haystack) {
		return false
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
