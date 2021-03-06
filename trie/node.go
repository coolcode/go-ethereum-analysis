// Copyright 2014 The github.com/go-ethereum-analysis Authors
// This file is part of the github.com/go-ethereum-analysis library.
//
// The github.com/go-ethereum-analysis library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The github.com/go-ethereum-analysis library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the github.com/go-ethereum-analysis library. If not, see <http://www.gnu.org/licenses/>.

package trie

import (
	"fmt"
	"io"
	"strings"

	"github.com/go-ethereum-analysis/common"
	"github.com/go-ethereum-analysis/rlp"
)

var indices = []string{"0", "1", "2", "3", "4", "5", "6", "7", "8", "9", "a", "b", "c", "d", "e", "f", "[17]"}

type node interface {
	fstring(string) string
	cache() (hashNode, bool)
	canUnload(cachegen, cachelimit uint16) bool
}

type (

	/**
	todo fullNode: 一个可以携带多个子节点的节点 root肯定是 fullNode

	1) 它有一个容量为 17 的 node 数组成员变量 Children
	2) 数组中前 16 个空位分别对应 16 进制 (hex) 下的 0-9a-f，这样对于每个子节点，
		根据其 key 值 16 进制形式下的第一位的值，
		就可挂载到 Children 数组的某个位置，fullNode 本身不再需要额外 key 变量；
	3) Children 数组的第 17 位，留给该 fullNode 的数据部分。
		fullNode 明显继承了原生 trie 的特点，而每个父节点最多拥有 16 个分支也包含了基于总体效率的考量.

	 */
	fullNode struct {

		// 实际的Trie节点数据进行编码/解码（需要自定义编码器）
		Children [17]node // Actual trie node data to encode/decode (needs custom encoder)

		// nodeFlag: 包含有关节点的与缓存相关的元数据, todo 其中包含了 hashNode
		flags    nodeFlag
	}

	/**
	todo  shortNode: 是一个仅有一个子节点的节点 (其实被包含的子节点就是 valueNode 了)

	1) 它的成员变量 Val 指向一个子节点，而成员 Key 是一个字节数组[]byte <这个就是真实的完整的key了>
	2) 显然 shortNode 的设计体现了 PatriciaTrie 的特点，通过合并只有一个子节点的父节点和其子节点来缩短 trie 的深度.
		(就是说将具备单叶子节点的父节点合并成了一个 shortNode)
	 */
	shortNode struct {

		// 这个是真正的 key
		Key   []byte

		// 其实这个是 valueNode
		Val   node

		// nodeFlag: 包含有关节点的与缓存相关的元数据, todo 其中包含了 hashNode
		flags nodeFlag
	}


	/**
	todo 这个十分的特殊

	1) hashNode 跟 valueNode 一样，也是字符数组 []byte 的一个别名，同样存放 32byte 的哈希值，也没有子节点。

	2) 不同的是，hashNode 是 todo fullNode 或者 shortNode 对象的 RLP 哈希值，
		所以它跟 valueNode 在使用上有着莫大的不同.


	一旦 fullNode 或 shortNode 的成员变量 (包括子结构) 发生任何变化，它们的 hashNode 就一定需要更新.

	在 trie.Trie 结构体的 insert()，delete()等函数实现中，
	可以看到除了新创建的 fullNode、shortNode，
	那些子结构有所改变的 fullNode、shortNode 的 nodeFlag 成员也会被重设，
	hashNode 会被清空。在下次 trie.Hash()调用时，整个 MPT 自底向上的遍历过程中，
	所有清空的 hashNode 会被重新赋值。这样 trie.Hash()结束后，
	我们可以得到一个根节点 root 的 hashNode，它就是此时此刻这个 MPT 结构的哈希值。
	上文中提到的，Block 的成员变量 Root、TxHash、ReceiptHash 的生成，正是源出于此.

	 */
	hashNode  []byte

	/**
	todo 承载了MPT结构中 真正数据部分的节点

	1) 它其实是字节数组 []byte 的一个别名，不带子节点。
	2) 在使用中，valueNode 就是所携带数据部分的 RLP 哈希值，长度 32byte，
		数据的 RLP 编码值作为 valueNode 的匹配项存储在数据库里.

	 */
	valueNode []byte
)

// nilValueNode is used when collapsing internal trie nodes for hashing, since
// unset children need to serialize correctly.
var nilValueNode = valueNode(nil)

// EncodeRLP encodes a full node into the consensus RLP format.
func (n *fullNode) EncodeRLP(w io.Writer) error {
	var nodes [17]node

	for i, child := range &n.Children {
		if child != nil {
			nodes[i] = child
		} else {
			nodes[i] = nilValueNode
		}
	}
	return rlp.Encode(w, nodes)
}

func (n *fullNode) copy() *fullNode   { copy := *n; return &copy }
func (n *shortNode) copy() *shortNode { copy := *n; return &copy }

// nodeFlag contains caching-related metadata about a node.
//
// nodeFlag: 包含有关节点的与缓存相关的元数据
type nodeFlag struct {
	// 节点的缓存哈希（可能为nil）
	hash  hashNode // cached hash of the node (may be nil)
	// 缓存生成计数器
	gen   uint16   // cache generation counter
	// 节点是否具有必须写入数据库的更改 (一个标识位)
	// 新创建并写入的节点的dirty值是为true的
	dirty bool     // whether the node has changes that must be written to the database
}

// canUnload tells whether a node can be unloaded.
func (n *nodeFlag) canUnload(cachegen, cachelimit uint16) bool {
	return !n.dirty && cachegen-n.gen >= cachelimit
}

func (n *fullNode) canUnload(gen, limit uint16) bool  { return n.flags.canUnload(gen, limit) }
func (n *shortNode) canUnload(gen, limit uint16) bool { return n.flags.canUnload(gen, limit) }
func (n hashNode) canUnload(uint16, uint16) bool      { return false }
func (n valueNode) canUnload(uint16, uint16) bool     { return false }

func (n *fullNode) cache() (hashNode, bool)  { return n.flags.hash, n.flags.dirty }
func (n *shortNode) cache() (hashNode, bool) { return n.flags.hash, n.flags.dirty }
func (n hashNode) cache() (hashNode, bool)   { return nil, true }
func (n valueNode) cache() (hashNode, bool)  { return nil, true }

// Pretty printing.
func (n *fullNode) String() string  { return n.fstring("") }
func (n *shortNode) String() string { return n.fstring("") }
func (n hashNode) String() string   { return n.fstring("") }
func (n valueNode) String() string  { return n.fstring("") }

func (n *fullNode) fstring(ind string) string {
	resp := fmt.Sprintf("[\n%s  ", ind)
	for i, node := range &n.Children {
		if node == nil {
			resp += fmt.Sprintf("%s: <nil> ", indices[i])
		} else {
			resp += fmt.Sprintf("%s: %v", indices[i], node.fstring(ind+"  "))
		}
	}
	return resp + fmt.Sprintf("\n%s] ", ind)
}
func (n *shortNode) fstring(ind string) string {
	return fmt.Sprintf("{%x: %v} ", n.Key, n.Val.fstring(ind+"  "))
}
func (n hashNode) fstring(ind string) string {
	return fmt.Sprintf("<%x> ", []byte(n))
}
func (n valueNode) fstring(ind string) string {
	return fmt.Sprintf("%x ", []byte(n))
}

func mustDecodeNode(hash, buf []byte, cachegen uint16) node {
	n, err := decodeNode(hash, buf, cachegen)
	if err != nil {
		panic(fmt.Sprintf("node %x: %v", hash, err))
	}
	return n
}

// decodeNode parses the RLP encoding of a trie node.
func decodeNode(hash, buf []byte, cachegen uint16) (node, error) {
	if len(buf) == 0 {
		return nil, io.ErrUnexpectedEOF
	}
	elems, _, err := rlp.SplitList(buf)
	if err != nil {
		return nil, fmt.Errorf("decode error: %v", err)
	}

	//
	switch c, _ := rlp.CountValues(elems); c {
	//
	case 2:
		n, err := decodeShort(hash, elems, cachegen)
		return n, wrapError(err, "short")

	//
	case 17:
		n, err := decodeFull(hash, elems, cachegen)
		return n, wrapError(err, "full")
	default:
		return nil, fmt.Errorf("invalid number of list elements: %v", c)
	}
}

func decodeShort(hash, elems []byte, cachegen uint16) (node, error) {
	kbuf, rest, err := rlp.SplitString(elems)
	if err != nil {
		return nil, err
	}
	flag := nodeFlag{hash: hash, gen: cachegen}
	key := compactToHex(kbuf)
	if hasTerm(key) {
		// value node
		val, _, err := rlp.SplitString(rest)
		if err != nil {
			return nil, fmt.Errorf("invalid value node: %v", err)
		}
		return &shortNode{key, append(valueNode{}, val...), flag}, nil
	}
	r, _, err := decodeRef(rest, cachegen)
	if err != nil {
		return nil, wrapError(err, "val")
	}
	return &shortNode{key, r, flag}, nil
}

func decodeFull(hash, elems []byte, cachegen uint16) (*fullNode, error) {
	n := &fullNode{flags: nodeFlag{hash: hash, gen: cachegen}}
	for i := 0; i < 16; i++ {
		cld, rest, err := decodeRef(elems, cachegen)
		if err != nil {
			return n, wrapError(err, fmt.Sprintf("[%d]", i))
		}
		n.Children[i], elems = cld, rest
	}
	val, _, err := rlp.SplitString(elems)
	if err != nil {
		return n, err
	}
	if len(val) > 0 {
		n.Children[16] = append(valueNode{}, val...)
	}
	return n, nil
}

const hashLen = len(common.Hash{})

func decodeRef(buf []byte, cachegen uint16) (node, []byte, error) {
	kind, val, rest, err := rlp.Split(buf)
	if err != nil {
		return nil, buf, err
	}
	switch {
	case kind == rlp.List:
		// 'embedded' node reference. The encoding must be smaller
		// than a hash in order to be valid.
		if size := len(buf) - len(rest); size > hashLen {
			err := fmt.Errorf("oversized embedded node (size is %d bytes, want size < %d)", size, hashLen)
			return nil, buf, err
		}
		n, err := decodeNode(nil, buf, cachegen)
		return n, rest, err
	case kind == rlp.String && len(val) == 0:
		// empty node
		return nil, rest, nil
	case kind == rlp.String && len(val) == 32:
		return append(hashNode{}, val...), rest, nil
	default:
		return nil, nil, fmt.Errorf("invalid RLP string size %d (want 0 or 32)", len(val))
	}
}

// wraps a decoding error with information about the path to the
// invalid child node (for debugging encoding issues).
type decodeError struct {
	what  error
	stack []string
}

func wrapError(err error, ctx string) error {
	if err == nil {
		return nil
	}
	if decErr, ok := err.(*decodeError); ok {
		decErr.stack = append(decErr.stack, ctx)
		return decErr
	}
	return &decodeError{err, []string{ctx}}
}

func (err *decodeError) Error() string {
	return fmt.Sprintf("%v (decode path: %s)", err.what, strings.Join(err.stack, "<-"))
}
