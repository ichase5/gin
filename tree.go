// Copyright 2013 Julien Schmidt. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be found
// at https://github.com/julienschmidt/httprouter/blob/master/LICENSE

package gin

import (
	"bytes"
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/gin-gonic/gin/internal/bytesconv"
)

var (
	strColon = []byte(":")
	strStar  = []byte("*")
	strSlash = []byte("/")
)

// Param is a single URL parameter, consisting of a key and a value.
type Param struct {
	Key   string
	Value string
}

// Params is a Param-slice, as returned by the router.
// The slice is ordered, the first URL parameter is also the first slice value.
// It is therefore safe to read values by the index.
type Params []Param

// Get returns the value of the first Param which key matches the given name and a boolean true.
// If no matching Param is found, an empty string is returned and a boolean false .
func (ps Params) Get(name string) (string, bool) {
	for _, entry := range ps {
		if entry.Key == name {
			return entry.Value, true
		}
	}
	return "", false
}

// ByName returns the value of the first Param which key matches the given name.
// If no matching Param is found, an empty string is returned.
func (ps Params) ByName(name string) (va string) {
	va, _ = ps.Get(name)
	return
}

type methodTree struct {
	method string
	root   *node
}

type methodTrees []methodTree

func (trees methodTrees) get(method string) *node {
	for _, tree := range trees {
		if tree.method == method {
			return tree.root
		}
	}
	return nil
}

func min(a, b int) int {
	if a <= b {
		return a
	}
	return b
}

func longestCommonPrefix(a, b string) int {
	i := 0
	max := min(len(a), len(b))
	for i < max && a[i] == b[i] {
		i++
	}
	return i
}

// 加到最后面，但是如果已经有wildcard子节点，要保持wild card子节点在最后面
func (n *node) addChild(child *node) {
	if n.wildChild && len(n.children) > 0 {
		wildcardChild := n.children[len(n.children)-1]
		n.children = append(n.children[:len(n.children)-1], child, wildcardChild)
	} else {
		n.children = append(n.children, child)
	}
}

func countParams(path string) uint16 {
	var n uint16
	s := bytesconv.StringToBytes(path)
	n += uint16(bytes.Count(s, strColon))
	n += uint16(bytes.Count(s, strStar))
	return n
}

func countSections(path string) uint16 {
	s := bytesconv.StringToBytes(path)
	return uint16(bytes.Count(s, strSlash))
}

type nodeType uint8

const (
	static nodeType = iota
	root
	param    // 对于 path 包含冒号通配符的情况，nType 是 param 类型
	catchAll // *通配符
)

type node struct {
	path      string
	indices   string
	wildChild bool // 其实就是说是否有通配类型的子节点，即有子节点的nType为param 或 catchAll
	nType     nodeType
	priority  uint32  // 以node为根节点的子树中，注册过的路径个数
	children  []*node // child nodes, at most 1 :param style node at the end of the array
	handlers  HandlersChain
	fullPath  string
}

// Increments priority of the given child and reorders if necessary
// children要保持按照priority从大到小排序
func (n *node) incrementChildPrio(pos int) int {
	cs := n.children
	cs[pos].priority++
	prio := cs[pos].priority

	// Adjust position (move to front)
	newPos := pos
	for ; newPos > 0 && cs[newPos-1].priority < prio; newPos-- {
		// Swap node positions
		cs[newPos-1], cs[newPos] = cs[newPos], cs[newPos-1]
	}

	// Build new index char string
	// a   b   c   d   e
	// 9   7   7   6   5
	// 9   7   7   8   5
	// a   d   b   c   e
	//  new_pos   pos
	if newPos != pos {
		// 对应上图中的四个部分: a d bc e
		n.indices = n.indices[:newPos] + // Unchanged prefix, might be empty
			n.indices[pos:pos+1] + // The index char we move
			n.indices[newPos:pos] + n.indices[pos+1:] // Rest without char at 'pos'
	}

	return newPos
}

// addRoute adds a node with the given handle to the path.
// Not concurrency-safe!
func (n *node) addRoute(path string, handlers HandlersChain) {
	// path已经包含了routerGroup的path作为前缀了，path就是完整的full path
	fullPath := path // 单独拷贝，因为path后面会被修改
	n.priority++

	// Empty tree（第一次在注册该method的路由）
	if len(n.path) == 0 && len(n.children) == 0 {
		n.insertChild(path, fullPath, handlers)
		n.nType = root
		return
	}

	parentFullPathIndex := 0 // 就是为了n.fullPath = fullPath[:parentFullPathIndex+i]使用。每经过一个child node就增加一次

walk:
	for {
		// Find the longest common prefix.
		// This also implies that the common prefix contains no ':' or '*'
		// since the existing key can't contain those chars.
		i := longestCommonPrefix(path, n.path) // i是第一个不相同字符的位置

		/*
						就这么几种情况:
						1 n.path = abc, path = xyz 或者path = abd
						2 n.path = abc, path = abcde
						3 n.path = abcd, path = abc
			   			4 n.path = abc, path = abc
		*/

		// Split node
		/*
			情况1或者情况3都会走到这里:
		*/
		if i < len(n.path) {
			// child会继承n的child nodes
			child := node{
				path:      n.path[i:],
				wildChild: n.wildChild,
				nType:     static,
				indices:   n.indices,
				children:  n.children,
				handlers:  n.handlers,
				priority:  n.priority - 1, // 分裂之前n本身的priority要减回去(line 155加上去的)
				fullPath:  n.fullPath,
			}

			n.children = []*node{&child}
			// []byte for proper unicode char conversion, see #65
			n.indices = bytesconv.BytesToString([]byte{n.path[i]})
			n.path = path[:i]
			n.handlers = nil
			n.wildChild = false
			n.fullPath = fullPath[:parentFullPathIndex+i]
		}

		/*
			情况1或者情况2，都会走到这里
			1 n.path = abc, path = xyz 或者path = abd
			2 n.path = abc, path = abcde
		*/
		// Make new node a child of this node
		if i < len(path) {
			// 超出去的部分
			path = path[i:] // 要插入的new child的path
			c := path[0]

			// '/' after param
			// 当前节点为参数节点并且只有一个子节点 && path以/开头, 那么我们应该递归到子节点
			// 参数节点不做匹配，直接用子节点进行匹配
			// 即 :id/abc, 然后插入 /xxx。也就是说，认为参数节点为空就匹配上了
			// 如果子节点多于一个，就没法直接选择子节点了
			if n.nType == param && c == '/' && len(n.children) == 1 {
				parentFullPathIndex += len(n.path)
				n = n.children[0]
				n.priority++
				continue walk
			}

			// Check if a child with the next path byte exists
			for i, max := 0, len(n.indices); i < max; i++ {
				if c == n.indices[i] {
					// 进入子节点了
					parentFullPathIndex += len(n.path)
					i = n.incrementChildPrio(i) // 保证子节点按照priority排序
					n = n.children[i]
					continue walk
				}
			}

			// 没有匹配上的子节点，需要插入new child
			if c != ':' && c != '*' && n.nType != catchAll { // TODO:?
				// []byte for proper unicode char conversion, see #65
				n.indices += bytesconv.BytesToString([]byte{c})
				child := &node{
					fullPath: fullPath,
				}
				n.addChild(child)
				n.incrementChildPrio(len(n.indices) - 1)
				n = child

				n.insertChild(path, fullPath, handlers) // insert child是insert child if needed
				return
			} else if n.wildChild { // TODO: 分析这种特殊情况 c=:或者c=*
				// inserting a wildcard node, need to check if it conflicts with the existing wildcard
				n = n.children[len(n.children)-1] // wildcard子节点一定是最后一个child node
				n.priority++

				// Check if the wildcard matches
				if len(path) >= len(n.path) && n.path == path[:len(n.path)] &&
					// Adding a child to a catchAll is not possible
					// catch all已经是末尾了，后面不能有东西
					n.nType != catchAll &&
					// Check for longer wildcard, e.g. :name and :names
					(len(n.path) >= len(path) || path[len(n.path)] == '/') {
					// ":id"类型通配符
					continue walk
				}

				// Wildcard conflict
				pathSeg := path
				if n.nType != catchAll {
					pathSeg = strings.SplitN(pathSeg, "/", 2)[0]
				}
				prefix := fullPath[:strings.Index(fullPath, pathSeg)] + n.path
				panic("'" + pathSeg +
					"' in new path '" + fullPath +
					"' conflicts with existing wildcard '" + n.path +
					"' in existing prefix '" + prefix +
					"'")
			}
		}

		if n.handlers != nil { // 情况4，重复注册了路由
			panic("handlers are already registered for path '" + fullPath + "'")
		}
		// 情况3在这里结束
		// 3 n.path = abcd, path = abc
		n.handlers = handlers
		n.fullPath = fullPath
		return
	}
}

// Search for a wildcard segment and check the name for invalid characters.
// Returns -1 as index, if no wildcard was found.
func findWildcard(path string) (wildcard string, i int, valid bool) {
	// Find start
	for start, c := range []byte(path) {
		// A wildcard starts with ':' (param) or '*' (catch-all)
		if c != ':' && c != '*' {
			continue
		}

		// Find end and check for invalid characters
		valid = true
		for end, c := range []byte(path[start+1:]) {
			switch c {
			case '/':
				return path[start : start+1+end], start, valid
			case ':', '*':
				// 不能在"/"出现前再次出现:或者*
				valid = false
			}
		}
		return path[start:], start, valid
	}
	return "", -1, false
}

func (n *node) insertChild(path string, fullPath string, handlers HandlersChain) {
	for {
		// Find prefix until first wildcard
		wildcard, i, valid := findWildcard(path)

		// The wildcard name must only contain one ':' or '*' character
		if !valid {
			panic("only one wildcard per path segment is allowed, has: '" +
				wildcard + "' in path '" + fullPath + "'")
		}

		if i < 0 { // No wildcard found
			break
		}

		// check if the wildcard has a name
		//必须是/:id 或者/*id 这种形式，不能是/: 或者/*这种形式,也就是说,:或者*后面必须有名字
		if len(wildcard) < 2 {
			panic("wildcards must be named with a non-empty name in path '" + fullPath + "'")
		}

		// 下面对于“:”和“*”的处理不同，两个逻辑块

		if wildcard[0] == ':' { // param
			// 有通配符，要拆成两部分（或者三个部分）
			// 通配符前，通配符，通配符后

			// 通配符前
			if i > 0 {
				// Insert prefix before the current wildcard
				n.path = path[:i]
				path = path[i:]
			}
			// 通配符部分要单独作为子节点
			child := &node{
				nType:    param,
				path:     wildcard, // 如":id"
				fullPath: fullPath,
			}
			n.addChild(child)
			n.wildChild = true
			n = child
			n.priority++

			// if the path doesn't end with the wildcard, then there
			// will be another subpath starting with '/'
			if len(wildcard) < len(path) { // 后面还有
				path = path[len(wildcard):]

				child := &node{
					priority: 1,
					fullPath: fullPath,
				}
				// 此时n已经是通配符部分了
				n.addChild(child)
				n = child
				continue
			}

			// Otherwise we're done. Insert the handle in the new leaf
			n.handlers = handlers
			return
		}

		// catchAll
		// /*id这种形式会走到下面的路径
		/*
			catch all的*id后面不能再有东西了
			catch all分为三个部分，前面，empty path(indices="/")，通配符部分
		*/

		// path不合法: /*id必须在url结尾，后面不能再有其他路径
		if i+len(wildcard) != len(path) {
			panic("catch-all routes are only allowed at the end of the path in path '" + fullPath + "'")
		}

		// "path/test" 和 要插入的"path/*name" 冲突。因为/path/test无法确定匹配哪个了
		if len(n.path) > 0 && n.path[len(n.path)-1] == '/' {
			pathSeg := strings.SplitN(n.children[0].path, "/", 2)[0] // 调用前保证n一定有child node
			panic("catch-all wildcard '" + path +
				"' in new path '" + fullPath +
				"' conflicts with existing path segment '" + pathSeg +
				"' in existing prefix '" + n.path + pathSeg +
				"'")
		}

		// currently fixed width 1 for '/'
		i--
		if path[i] != '/' {
			// *前面必须有/
			panic("no / before catch-all in path '" + fullPath + "'")
		}

		// 通配符前部分
		n.path = path[:i]

		// empty path部分
		// First node: catchAll node with empty path
		child := &node{
			wildChild: true,
			nType:     catchAll,
			fullPath:  fullPath,
		}

		n.addChild(child)
		n.indices = string('/')
		n = child
		n.priority++

		// 通配符部分
		child = &node{
			path:     path[i:], // "/*name"
			nType:    catchAll,
			handlers: handlers,
			priority: 1,
			fullPath: fullPath,
		}
		n.children = []*node{child}

		return
	}

	// If no wildcard was found, simply insert the path and handle
	/*
		没有通配符的，很简单，不需要创建新的child node,直接在当前node上添加handler即可
	*/
	n.path = path
	n.handlers = handlers
	n.fullPath = fullPath
}

// nodeValue holds return values of (*Node).getValue method
type nodeValue struct {
	handlers HandlersChain
	params   *Params // wildcard params
	tsr      bool    // 多一个/的路径存在，尝试redirect
	fullPath string
}

type skippedNode struct {
	path        string
	node        *node
	paramsCount int16
}

// Returns the handle registered with the given path (key). The values of
// wildcards are saved to a map.
// If no handle can be found, a TSR (trailing slash redirect) recommendation is
// made if a handle exists with an extra (without the) trailing slash for the
// given path.
func (n *node) getValue(path string, params *Params, skippedNodes *[]skippedNode, unescape bool) (value nodeValue) {
	var globalParamsCount int16 // TODO: ?

walk: // Outer loop for walking the tree
	for {
		prefix := n.path
		if len(path) > len(prefix) && path[:len(prefix)] == prefix {
			// 当前node命中了，该查找child nodes了
			path = path[len(prefix):]

			// Try all the non-wildcard children first by matching the indices
			idxc := path[0]
			for i, c := range []byte(n.indices) {
				if c == idxc {
					//  strings.HasPrefix(n.children[len(n.children)-1].path, ":") == n.wildChild
					// TODO: ??
					if n.wildChild {
						index := len(*skippedNodes)
						*skippedNodes = (*skippedNodes)[:index+1]
						(*skippedNodes)[index] = skippedNode{
							path: prefix + path,
							node: &node{
								path:      n.path,
								indices:   "",
								wildChild: n.wildChild,
								nType:     n.nType,
								priority:  n.priority,
								children:  n.children,
								handlers:  n.handlers,
								fullPath:  n.fullPath,
							},
							paramsCount: globalParamsCount,
						}
					}

					// 进入子节点
					n = n.children[i]
					continue walk
				}
			}

			if !n.wildChild {
				// If the path at the end of the loop is not equal to '/' and the current node has no child nodes
				// the current node needs to roll back to last valid skippedNode
				if path != "/" {
					for length := len(*skippedNodes); length > 0; length-- {
						skippedNode := (*skippedNodes)[length-1]
						*skippedNodes = (*skippedNodes)[:length-1]
						if strings.HasSuffix(skippedNode.path, path) {
							path = skippedNode.path
							n = skippedNode.node
							if value.params != nil {
								*value.params = (*value.params)[:skippedNode.paramsCount]
							}
							globalParamsCount = skippedNode.paramsCount
							continue walk
						}
					}
				}

				// Nothing found.
				// We can recommend to redirect to the same URL without a
				// trailing slash if a leaf exists for that path.
				value.tsr = path == "/" && n.handlers != nil
				return
			}

			// Handle wildcard child, which is always at the end of the array
			n = n.children[len(n.children)-1]
			globalParamsCount++

			switch n.nType {
			case param:
				// fix truncate the parameter
				// tree_test.go  line: 204

				// Find param end (either '/' or path end)
				end := 0
				for end < len(path) && path[end] != '/' {
					end++
				}

				// Save param value
				if params != nil && cap(*params) > 0 {
					if value.params == nil {
						value.params = params
					}
					// Expand slice within preallocated capacity
					i := len(*value.params)
					*value.params = (*value.params)[:i+1]
					val := path[:end]
					if unescape {
						if v, err := url.QueryUnescape(val); err == nil {
							val = v
						}
					}
					(*value.params)[i] = Param{
						Key:   n.path[1:],
						Value: val,
					}
				}

				// we need to go deeper!
				if end < len(path) {
					if len(n.children) > 0 {
						path = path[end:]
						n = n.children[0]
						continue walk
					}

					// ... but we can't
					value.tsr = len(path) == end+1
					return
				}

				if value.handlers = n.handlers; value.handlers != nil {
					value.fullPath = n.fullPath
					return
				}
				if len(n.children) == 1 {
					// No handle found. Check if a handle for this path + a
					// trailing slash exists for TSR recommendation
					n = n.children[0]
					value.tsr = (n.path == "/" && n.handlers != nil) || (n.path == "" && n.indices == "/")
				}
				return

			case catchAll:
				// Save param value
				if params != nil {
					if value.params == nil {
						value.params = params
					}
					// Expand slice within preallocated capacity
					i := len(*value.params)
					*value.params = (*value.params)[:i+1]
					val := path
					if unescape {
						if v, err := url.QueryUnescape(path); err == nil {
							val = v
						}
					}
					(*value.params)[i] = Param{
						Key:   n.path[2:],
						Value: val,
					}
				}

				value.handlers = n.handlers
				value.fullPath = n.fullPath
				return

			default:
				panic("invalid node type")
			}
		}

		// 相同
		if path == prefix {
			// If the current path does not equal '/' and the node does not have a registered handle and the most recently matched node has a child node
			// the current node needs to roll back to last valid skippedNode
			// TODO: 什么？
			if n.handlers == nil && path != "/" {
				for length := len(*skippedNodes); length > 0; length-- {
					skippedNode := (*skippedNodes)[length-1]
					*skippedNodes = (*skippedNodes)[:length-1]
					if strings.HasSuffix(skippedNode.path, path) {
						path = skippedNode.path
						n = skippedNode.node
						if value.params != nil {
							*value.params = (*value.params)[:skippedNode.paramsCount]
						}
						globalParamsCount = skippedNode.paramsCount
						continue walk
					}
				}
				//	n = latestNode.children[len(latestNode.children)-1]
			}
			// We should have reached the node containing the handle.
			// Check if this node has a handle registered.
			if value.handlers = n.handlers; value.handlers != nil { // 找到了,直接返回
				value.fullPath = n.fullPath
				return
			}

			// If there is no handle for this route, but this route has a
			// wildcard child, there must be a handle for this path with an
			// additional trailing slash
			// wildchild,即:或者*的节点，自然可以接受/
			if path == "/" && n.wildChild && n.nType != root {
				value.tsr = true
				return
			}

			// TODO: 这个是什么case?
			if path == "/" && n.nType == static {
				value.tsr = true
				return
			}

			// No handle found. Check if a handle for this path + a
			// trailing slash exists for trailing slash recommendation
			// 找tsr的机会
			for i, c := range []byte(n.indices) {
				if c == '/' {
					n = n.children[i]
					value.tsr = (len(n.path) == 1 && n.handlers != nil) || // 单纯后面多一个"/"的路径存在
						(n.nType == catchAll && n.children[0].handlers != nil) // 后面多"/*"的路径存在
					return
				}
			}

			return
		}

		// 匹配不上，才会进入下面的逻辑

		// Nothing found. We can recommend to redirect to the same URL with an
		// extra trailing slash if a leaf exists for that path
		value.tsr = path == "/" || // TODO: 为什么path == "/"就可以tsr??
			// path刚好是prefix + "/",且存在handler
			(len(prefix) == len(path)+1 && prefix[len(path)] == '/' &&
				path == prefix[:len(prefix)-1] && n.handlers != nil)

		// roll back to last valid skippedNode
		if !value.tsr && path != "/" {
			for length := len(*skippedNodes); length > 0; length-- {
				skippedNode := (*skippedNodes)[length-1]
				*skippedNodes = (*skippedNodes)[:length-1]
				if strings.HasSuffix(skippedNode.path, path) {
					path = skippedNode.path
					n = skippedNode.node
					if value.params != nil {
						*value.params = (*value.params)[:skippedNode.paramsCount]
					}
					globalParamsCount = skippedNode.paramsCount
					continue walk
				}
			}
		}

		return
	}
}

// Makes a case-insensitive lookup of the given path and tries to find a handler.
// It can optionally also fix trailing slashes.
// It returns the case-corrected path and a bool indicating whether the lookup
// was successful.
func (n *node) findCaseInsensitivePath(path string, fixTrailingSlash bool) ([]byte, bool) {
	const stackBufSize = 128

	// Use a static sized buffer on the stack in the common case.
	// If the path is too long, allocate a buffer on the heap instead.
	buf := make([]byte, 0, stackBufSize)
	if length := len(path) + 1; length > stackBufSize {
		buf = make([]byte, 0, length)
	}

	ciPath := n.findCaseInsensitivePathRec(
		path,
		buf,       // Preallocate enough memory for new path
		[4]byte{}, // Empty rune buffer
		fixTrailingSlash,
	)

	return ciPath, ciPath != nil
}

// Shift bytes in array by n bytes left
func shiftNRuneBytes(rb [4]byte, n int) [4]byte {
	switch n {
	case 0:
		return rb
	case 1:
		return [4]byte{rb[1], rb[2], rb[3], 0}
	case 2:
		return [4]byte{rb[2], rb[3]}
	case 3:
		return [4]byte{rb[3]}
	default:
		return [4]byte{}
	}
}

// Recursive case-insensitive lookup function used by n.findCaseInsensitivePath
func (n *node) findCaseInsensitivePathRec(path string, ciPath []byte, rb [4]byte, fixTrailingSlash bool) []byte {
	npLen := len(n.path)

walk: // Outer loop for walking the tree
	for len(path) >= npLen && (npLen == 0 || strings.EqualFold(path[1:npLen], n.path[1:])) {
		// Add common prefix to result
		oldPath := path
		path = path[npLen:]
		ciPath = append(ciPath, n.path...)

		if len(path) == 0 {
			// We should have reached the node containing the handle.
			// Check if this node has a handle registered.
			if n.handlers != nil {
				return ciPath
			}

			// No handle found.
			// Try to fix the path by adding a trailing slash
			if fixTrailingSlash {
				for i, c := range []byte(n.indices) {
					if c == '/' {
						n = n.children[i]
						if (len(n.path) == 1 && n.handlers != nil) ||
							(n.nType == catchAll && n.children[0].handlers != nil) {
							return append(ciPath, '/')
						}
						return nil
					}
				}
			}
			return nil
		}

		// If this node does not have a wildcard (param or catchAll) child,
		// we can just look up the next child node and continue to walk down
		// the tree
		if !n.wildChild {
			// Skip rune bytes already processed
			rb = shiftNRuneBytes(rb, npLen)

			if rb[0] != 0 {
				// Old rune not finished
				idxc := rb[0]
				for i, c := range []byte(n.indices) {
					if c == idxc {
						// continue with child node
						n = n.children[i]
						npLen = len(n.path)
						continue walk
					}
				}
			} else {
				// Process a new rune
				var rv rune

				// Find rune start.
				// Runes are up to 4 byte long,
				// -4 would definitely be another rune.
				var off int
				for max := min(npLen, 3); off < max; off++ {
					if i := npLen - off; utf8.RuneStart(oldPath[i]) {
						// read rune from cached path
						rv, _ = utf8.DecodeRuneInString(oldPath[i:])
						break
					}
				}

				// Calculate lowercase bytes of current rune
				lo := unicode.ToLower(rv)
				utf8.EncodeRune(rb[:], lo)

				// Skip already processed bytes
				rb = shiftNRuneBytes(rb, off)

				idxc := rb[0]
				for i, c := range []byte(n.indices) {
					// Lowercase matches
					if c == idxc {
						// must use a recursive approach since both the
						// uppercase byte and the lowercase byte might exist
						// as an index
						if out := n.children[i].findCaseInsensitivePathRec(
							path, ciPath, rb, fixTrailingSlash,
						); out != nil {
							return out
						}
						break
					}
				}

				// If we found no match, the same for the uppercase rune,
				// if it differs
				if up := unicode.ToUpper(rv); up != lo {
					utf8.EncodeRune(rb[:], up)
					rb = shiftNRuneBytes(rb, off)

					idxc := rb[0]
					for i, c := range []byte(n.indices) {
						// Uppercase matches
						if c == idxc {
							// Continue with child node
							n = n.children[i]
							npLen = len(n.path)
							continue walk
						}
					}
				}
			}

			// Nothing found. We can recommend to redirect to the same URL
			// without a trailing slash if a leaf exists for that path
			if fixTrailingSlash && path == "/" && n.handlers != nil {
				return ciPath
			}
			return nil
		}

		n = n.children[0]
		switch n.nType {
		case param:
			// Find param end (either '/' or path end)
			end := 0
			for end < len(path) && path[end] != '/' {
				end++
			}

			// Add param value to case insensitive path
			ciPath = append(ciPath, path[:end]...)

			// We need to go deeper!
			if end < len(path) {
				if len(n.children) > 0 {
					// Continue with child node
					n = n.children[0]
					npLen = len(n.path)
					path = path[end:]
					continue
				}

				// ... but we can't
				if fixTrailingSlash && len(path) == end+1 {
					return ciPath
				}
				return nil
			}

			if n.handlers != nil {
				return ciPath
			}

			if fixTrailingSlash && len(n.children) == 1 {
				// No handle found. Check if a handle for this path + a
				// trailing slash exists
				n = n.children[0]
				if n.path == "/" && n.handlers != nil {
					return append(ciPath, '/')
				}
			}

			return nil

		case catchAll:
			return append(ciPath, path...)

		default:
			panic("invalid node type")
		}
	}

	// Nothing found.
	// Try to fix the path by adding / removing a trailing slash
	if fixTrailingSlash {
		if path == "/" {
			return ciPath
		}
		if len(path)+1 == npLen && n.path[len(path)] == '/' &&
			strings.EqualFold(path[1:], n.path[1:len(path)]) && n.handlers != nil {
			return append(ciPath, n.path...)
		}
	}
	return nil
}
