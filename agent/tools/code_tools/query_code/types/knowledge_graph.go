package types

import (
	"gonum.org/v1/gonum/graph"
	"gonum.org/v1/gonum/graph/simple"
)

// CodeGraph represents the in-memory knowledge graph
type CodeGraph struct {
	Graph          *simple.DirectedGraph
	NameToNodesMap map[string]*CodeNode // Map to track nodes by unique identifier
	IDsToNodesMap  map[int64]*CodeNode
}

// CodeNode represents a graph node for code elements
type CodeNode struct {
	id            int64  // Unique ID for the node
	Name          string // Unique Name of the element (<module>/<package>#<name>) if module exists else use folder
	Type          string // Type of the code element (e.g., function, import)
	Path          string // Path to the file containing the node
	StartPosition string // Position in the file (e.g., "5:0")
	EndPosition   string // Position in the file (e.g., "10:0")
	Description   string // Description of what the function does from OpenAI
}

type CodeEdge struct {
	Type     string
	FromNode *CodeNode
	ToNode   *CodeNode
}

func (c *CodeEdge) From() graph.Node {
	return c.FromNode
}

func (c *CodeEdge) To() graph.Node {
	return c.ToNode
}

func (c *CodeEdge) ReversedEdge() graph.Edge {
	//TODO implement me
	panic("implement me")
}

var _ graph.Node = &CodeNode{}
var _ graph.Edge = &CodeEdge{}

// ID satisfies the graph.Node interface
func (n *CodeNode) ID() int64 {
	return n.id
}

// NewCodeGraph initializes a new in-memory graph
func NewCodeGraph() *CodeGraph {
	return &CodeGraph{
		Graph:          simple.NewDirectedGraph(),
		NameToNodesMap: make(map[string]*CodeNode),
		IDsToNodesMap:  make(map[int64]*CodeNode),
	}
}

// AddNode adds a code element as a node to the graph
func (cg *CodeGraph) AddNode(name, nodeType, path, startPosition, endPosition string) *CodeNode {
	if _, exists := cg.NameToNodesMap[name]; exists {
		return cg.NameToNodesMap[name]
	}

	id := cg.Graph.NewNode().ID()
	node := &CodeNode{
		id:            id,
		Type:          nodeType,
		Name:          name,
		Path:          path,
		StartPosition: startPosition,
		EndPosition:   endPosition,
	}

	cg.NameToNodesMap[name] = node
	cg.IDsToNodesMap[id] = node
	cg.Graph.AddNode(node)
	return node
}

// AddEdge adds a directed relationship (edge) between two nodes
func (cg *CodeGraph) AddEdge(sourceID, targetID, edgeType string) {
	sourceNode, sourceExists := cg.NameToNodesMap[sourceID]
	targetNode, targetExists := cg.NameToNodesMap[targetID]

	if sourceExists && targetExists {
		edge := &CodeEdge{
			Type:     edgeType,
			FromNode: sourceNode,
			ToNode:   targetNode,
		}
		cg.Graph.SetEdge(edge)
	}
}
