package utils

func MatchRoute(pattern, path string) (bool, map[string]string) {
	params := make(map[string]string)
	pi, ti := 0, 0
	pLen, tLen := len(pattern), len(path)
	skipSlash := func(s string, i int) int {
		for i < len(s) && s[i] == '/' {
			i++
		}
		return i
	}
	pi = skipSlash(pattern, pi)
	ti = skipSlash(path, ti)
	for pi < pLen && ti < tLen {
		switch pattern[pi] {
		case ':':
			startName := pi + 1
			for pi < pLen && pattern[pi] != '/' {
				pi++
			}
			paramName := pattern[startName:pi]
			startVal := ti
			for ti < tLen && path[ti] != '/' {
				ti++
			}
			paramVal := path[startVal:ti]
			params[paramName] = paramVal
		case '*':
			pi++
			if pi < pLen && pattern[pi] == '/' {
				pi++
			}
			paramName := pattern[pi:]
			paramVal := path[ti:]
			params[paramName] = paramVal
			ti = tLen
			pi = pLen
			break
		default:
			for pi < pLen && ti < tLen && pattern[pi] != '/' && path[ti] != '/' {
				if pattern[pi] != path[ti] {
					return false, nil
				}
				pi++
				ti++
			}
		}
		pi = skipSlash(pattern, pi)
		ti = skipSlash(path, ti)
	}
	if pi == pLen && ti == tLen {
		return true, params
	}
	return false, nil
}
