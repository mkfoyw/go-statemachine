package statemachine

import (
	"context"
	"reflect"
	"sync/atomic"

	"github.com/filecoin-project/go-statestore"
	xerrors "golang.org/x/xerrors"

	logging "github.com/ipfs/go-log"
)

var log = logging.Logger("evtsm")

var ErrTerminated = xerrors.New("normal shutdown of state machine")

// Event 发送进入状态机的事件，
type Event struct {
	User interface{}
}

// Planner processes in queue events
// It returns:
// 1. a handler of type -- func(ctx Context, st <T>) (func(*<T>), error), where <T> is the typeOf(User) param
// 2. the number of events processed
// 3. an error if occured
// 状态机的事件处理函数， 处理进入状态机的内部事件
type Planner func(events []Event, user interface{}) (interface{}, uint64, error)

// 状态机
type StateMachine struct {
	//状态机的事件处理函数
	planner Planner
	//输入事件队列
	eventsIn chan Event

	// 状态机的id
	name interface{}
	// 存储状态机的内部状态
	st *statestore.StoredState
	// 状态机内部保存数据的数据结构类型
	stateType reflect.Type

	// 已经执行完毕该阶段的退出函数
	stageDone chan struct{}
	// 状态机正在开始关闭
	closing chan struct{}
	// 状态机已经关闭
	closed chan struct{}

	// 状态机繁忙， 正在运行事件处理函数
	busy int32
}

func (fsm *StateMachine) run() {
	defer close(fsm.closed)

	var pendingEvents []Event

	for {
		// NOTE: This requires at least one event to be sent to trigger a stage
		//  This means that after restarting the state machine users of this
		//  code must send a 'restart' event
		select {
		case evt := <-fsm.eventsIn:
			pendingEvents = append(pendingEvents, evt)
		case <-fsm.stageDone:
			if len(pendingEvents) == 0 {
				continue
			}
		case <-fsm.closing:
			return
		}

		if atomic.CompareAndSwapInt32(&fsm.busy, 0, 1) {
			var nextStep interface{}
			var ustate interface{}
			var processed uint64
			var terminated bool

			err := fsm.mutateUser(func(user interface{}) (err error) {
				nextStep, processed, err = fsm.planner(pendingEvents, user)
				ustate = user
				if xerrors.Is(err, ErrTerminated) {
					terminated = true
					return nil
				}
				return err
			})
			if terminated {
				return
			}
			if err != nil {
				log.Errorf("Executing event planner failed: %+v", err)
				return
			}

			if processed < uint64(len(pendingEvents)) {
				pendingEvents = pendingEvents[processed:]
			} else {
				pendingEvents = nil
			}

			ctx := Context{
				ctx: context.TODO(),
				send: func(evt interface{}) error {
					return fsm.send(Event{User: evt})
				},
			}

			go func() {
				// 执行状态机的退出函数
				if nextStep != nil {
					res := reflect.ValueOf(nextStep).Call([]reflect.Value{reflect.ValueOf(ctx), reflect.ValueOf(ustate).Elem()})

					if res[0].Interface() != nil {
						log.Errorf("executing step: %+v", res[0].Interface().(error)) // TODO: propagate top level
						return
					}
				}
				atomic.StoreInt32(&fsm.busy, 0)
				fsm.stageDone <- struct{}{}
			}()

		}
	}
}

// mutateUser 修改状态机内部状态数据
func (fsm *StateMachine) mutateUser(cb func(user interface{}) error) error {
	// 生成状态机的输入函数函数类型
	mutt := reflect.FuncOf([]reflect.Type{reflect.PtrTo(fsm.stateType)}, []reflect.Type{reflect.TypeOf(new(error)).Elem()}, false)

	// 创建输入动作函数类型
	mutf := reflect.MakeFunc(mutt, func(args []reflect.Value) (results []reflect.Value) {
		err := cb(args[0].Interface())
		return []reflect.Value{reflect.ValueOf(&err).Elem()}
	})

	// 输入动作函数开始修改状态机的内部函数
	return fsm.st.Mutate(mutf.Interface())
}

// 发送一个事件到状态机
func (fsm *StateMachine) send(evt Event) error {
	select {
	case <-fsm.closed:
		return ErrTerminated
	case fsm.eventsIn <- evt: // TODO: ctx, at least
		return nil
	}
}

// stop 停止状态机
func (fsm *StateMachine) stop(ctx context.Context) error {
	close(fsm.closing)

	select {
	case <-fsm.closed:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
