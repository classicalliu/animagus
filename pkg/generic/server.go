package generic

import (
	"context"
	"fmt"
	"strings"

	"github.com/golang/protobuf/proto"
	"github.com/gomodule/redigo/redis"
	"github.com/machinebox/graphql"
	internal_ast "github.com/xxuejie/animagus/internal/ast"
	"github.com/xxuejie/animagus/pkg/ast"
	"github.com/xxuejie/animagus/pkg/coretypes"
	"github.com/xxuejie/animagus/pkg/executor"
	"github.com/xxuejie/animagus/pkg/indexer"
	"github.com/xxuejie/animagus/pkg/rpctypes"
)

type callInfo struct {
	expr    *ast.Value
	context indexer.ValueContext
}

type Server struct {
	calls         map[string]callInfo
	redisPool     *redis.Pool
	graphqlClient *graphql.Client
}

func NewServer(astContent []byte, redisPool *redis.Pool, graphqlUrl string) (*Server, error) {
	root := &ast.Root{}
	err := proto.Unmarshal(astContent, root)
	if err != nil {
		return nil, err
	}
	calls := make(map[string]callInfo)
	for _, call := range root.GetCalls() {
		valueContext, err := indexer.NewValueContext(call.GetName(), call.GetResult())
		if err != nil {
			return nil, err
		}
		calls[call.GetName()] = callInfo{
			expr:    call.GetResult(),
			context: valueContext,
		}
	}
	client := graphql.NewClient(graphqlUrl)
	return &Server{
		calls:         calls,
		redisPool:     redisPool,
		graphqlClient: client,
	}, nil
}

type executeEnvironment struct {
	params       *GenericParams
	valueContext indexer.ValueContext
	s            *Server
}

func (e executeEnvironment) Arg(i int) *internal_ast.Value {
	return nil
}

func (e executeEnvironment) Param(i int) *internal_ast.Value {
	if i < 0 || i >= len(e.params.GetParams()) {
		return nil
	}
	return &internal_ast.Value{
		Value: e.params.GetParams()[i],
	}
}

type getCellsResponse struct {
	GetCells []*rpctypes.OutPoint
}

func (e executeEnvironment) QueryCell(query *ast.List) ([]*internal_ast.Value, error) {
	queryIndex := e.valueContext.QueryIndex(query)
	if queryIndex == -1 {
		return nil, fmt.Errorf("Invalid query cell argument!")
	}
	params := make([]*internal_ast.Value, len(e.params.GetParams()))
	for i, param := range e.params.GetParams() {
		params[i] = &internal_ast.Value{
			Value: param,
		}
	}
	indexKey, err := e.valueContext.IndexKey(queryIndex, params)
	if err != nil {
		return nil, err
	}
	conn := e.s.redisPool.Get()
	defer conn.Close()
	slices, err := redis.ByteSlices(conn.Do("SMEMBERS", indexKey))
	if err != nil {
		return nil, err
	}
	if len(slices) == 0 {
		return []*internal_ast.Value{}, nil
	}
	outPoints := make([]coretypes.OutPoint, len(slices))
	for i, slice := range slices {
		outPoints[i] = coretypes.OutPoint(slice)
		if !outPoints[i].Verify(true) {
			return nil, fmt.Errorf("OutPoint %x verification failure!", slice)
		}
	}
	req := graphql.NewRequest(fmt.Sprintf(`
query {
  getCells(outPoints: %s) {
    cell {
      capacity
      lock {
        code_hash
        hash_type
        args
      }
    }
    cell_data {
      content
    }
  }
}
`, assembleQueryString(outPoints)))
	var response getCellsResponse
	err = e.s.graphqlClient.Run(context.Background(), req, &response)
	if err != nil {
		return nil, err
	}
	results := make([]*internal_ast.Value, len(response.GetCells))
	for i, cell := range response.GetCells {
		results[i] = &internal_ast.Value{
			Cell:     cell.GraphqlCell,
			CellData: cell.GraphqlCellData.Content,
		}
	}
	return results, nil
}

func assembleQueryString(outPoints []coretypes.OutPoint) string {
	pieces := make([]string, len(outPoints))
	for i, outPoint := range outPoints {
		pieces[i] = fmt.Sprintf("{txHash: \"0x%x\", index: \"0x%x\"}", outPoint.TxHash(), outPoint.Index())
	}
	return fmt.Sprintf("[%s]", strings.Join(pieces, ", "))
}

func (s *Server) Call(ctx context.Context, p *GenericParams) (*ast.Value, error) {
	callInfo, found := s.calls[p.GetName()]
	if !found {
		return nil, fmt.Errorf("Calling non-exist function: %s", p.GetName())
	}
	environment := executeEnvironment{
		params:       p,
		valueContext: callInfo.context,
		s:            s,
	}
	return executor.Execute(callInfo.expr, environment)
}