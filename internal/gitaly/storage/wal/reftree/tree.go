package reftree

import (
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

var (
	errParentNotFound       = errors.New("parent not found")
	errParentIsNotDirectory = errors.New("parent is not a directory")
	errTargetNotFound       = errors.New("target not found")
	errTargetAlreadyExists  = errors.New("target already exists")
	errTargetHasChildren    = errors.New("target has children")
)

type children map[string]*Node

// Node is a node in the reference tree.
type Node struct {
	// children are the children of this node. If this node is a directory,
	// children is not nil. If this node is a file, children is nil.
	children children
}

// New returns a new root node of a tree.
func New() *Node {
	return newDirectory()
}

func newFile() *Node {
	return &Node{}
}

func newDirectory() *Node {
	return &Node{children: children{}}
}

func (n *Node) isDirectory() bool {
	return n.children != nil
}

// InsertReference inserts a given reference to the tree. It creates any missing parent
// directories. Reference must be fully qualified.
func (n *Node) InsertReference(reference string) error {
	if !strings.HasPrefix(reference, "refs/") {
		return errors.New("expected a fully qualified reference")
	}

	return n.InsertNode(reference, true, false)
}

// InsertNode inserts a node into the tree at the given path optionally creating any
// missing parent directories.
func (n *Node) InsertNode(path string, createParents bool, isDirectory bool) error {
	var (
		pathPrefix string
		pathBase   = path
		node       = n
	)

	// Walk down the tree until we find the node.
	for {
		prefix, suffix, foundSeparator := strings.Cut(pathBase, "/")
		if !foundSeparator {
			// If there was no separator, then it means the current node
			// is the parent node of the inserted node.
			if node.children[pathBase] != nil {
				return errTargetAlreadyExists
			}

			targetNode := newFile()
			if isDirectory {
				targetNode = newDirectory()
			}

			node.children[pathBase] = targetNode

			return nil
		}

		// Since there was a separator, we still need to walk down the tree
		// to find parent node.
		child := node.children[prefix]
		if child == nil {
			if !createParents {
				return errParentNotFound
			}

			child = newDirectory()
			node.children[prefix] = child
		}

		if !child.isDirectory() {
			return errParentIsNotDirectory
		}

		node = child
		pathPrefix = filepath.Join(pathPrefix, prefix)
		pathBase = suffix
	}
}

// RemoveNode removes a node at the given path from the tree.
func (n *Node) RemoveNode(path string) error {
	var (
		pathPrefix string
		pathBase   = path
		node       = n
	)

	// Walk down the tree until we find the node of the reference.
	for {
		prefix, suffix, foundSeparator := strings.Cut(pathBase, "/")
		if !foundSeparator {
			// If there was no separator, then it means the current node
			// is the parent node of the reference that is modified.
			//
			// Get the target node.
			if targetNode, ok := node.children[pathBase]; !ok {
				return errTargetNotFound
			} else if len(targetNode.children) > 0 {
				return errTargetHasChildren
			}

			delete(node.children, pathBase)
			return nil
		}

		// Since there was a separator, we still need to walk down the tree
		// to find parent node.
		child := node.children[prefix]
		if child == nil {
			// As a parent of the target node doesn't exist, the target can't exist either.
			return errParentNotFound
		}

		if !child.isDirectory() {
			return errParentIsNotDirectory
		}

		node = child
		pathPrefix = filepath.Join(pathPrefix, prefix)
		pathBase = suffix
	}
}

// Contains returns whether or not the tree contains a node at the given path.
func (n *Node) Contains(path string) bool {
	var (
		pathPrefix string
		pathBase   = path
		node       = n
	)

	// Walk down the tree until we find the node of the reference.
	for {
		prefix, suffix, foundSeparator := strings.Cut(pathBase, "/")
		if !foundSeparator {
			// If there was no separator, then it means the current node
			// is the parent node of the reference that is modified.
			//
			// Check whether the target node exists.
			return node.children[pathBase] != nil
		}

		// Since there was a separator, we still need to walk down the tree
		// to find parent node.
		child := node.children[prefix]
		if child == nil {
			// Child node didn't exist. As a parent of the target node doesn't exist,
			// the target can't exist either.
			return false
		}

		if !child.isDirectory() {
			// This node was not a directory and can't be walked down.
			return false
		}

		node = child
		pathPrefix = filepath.Join(pathPrefix, prefix)
		pathBase = suffix
	}
}

// WalkCallback is a callback function that is invoked by the Walk* methods when
// walking the reference tree. Path is the path in the refs directory relative to
// the repository root, so for example 'refs' or 'refs/heads/main'. isDir indicates
// whether the path is a directory or not.
type WalkCallback func(path string, isDir bool) error

// WalkPreOrder walks the reference tree invoking the callback for each
// entry it finds. WalkPreOrder invokes the callback on directories
// before invoking it on the children of the directories.
func (n *Node) WalkPreOrder(callback WalkCallback) error {
	refs := n.children["refs"]
	if refs == nil {
		return nil
	}

	return refs.walk("refs", callback, true)
}

// WalkPostOrder walks the reference tree invoking the callback for each
// entry it finds. WalkPostOrder invokes the callback on directories
// after invoking it on the children of the directories.
func (n *Node) WalkPostOrder(callback WalkCallback) error {
	refs := n.children["refs"]
	if refs == nil {
		return nil
	}

	return refs.walk("refs", callback, false)
}

func (n *Node) walk(path string, callback WalkCallback, preOrder bool) error {
	isDir := n.children != nil

	if preOrder {
		if err := callback(path, isDir); err != nil {
			return fmt.Errorf("pre-order callback: %w", err)
		}
	}

	type child struct {
		path string
		node *Node
	}

	sortedChildren := make([]child, 0, len(n.children))
	for path, childNode := range n.children {
		sortedChildren = append(sortedChildren, child{path: path, node: childNode})
	}

	// Walk the children in lexicographical order to produce deterministic ordering.
	sort.Slice(sortedChildren, func(i, j int) bool {
		return sortedChildren[i].path < sortedChildren[j].path
	})

	for _, child := range sortedChildren {
		if err := child.node.walk(filepath.Join(path, child.path), callback, preOrder); err != nil {
			return fmt.Errorf("walk: %w", err)
		}
	}

	if !preOrder {
		if err := callback(path, isDir); err != nil {
			return fmt.Errorf("post-order callback: %w", err)
		}
	}

	return nil
}
