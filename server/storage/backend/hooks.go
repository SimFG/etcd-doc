// Copyright 2021 The etcd Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package backend

/*** TODO simfg confuse
这逻辑是真的绕，定义了一个接口，然后给这个接口定义了一个同样的func类型，然后创建一个struct
里面一个成员变量是这个func，然后去实现之前定义的接口
这里好像就是一种接口设计
*/

type HookFunc func(tx BatchTx)

// Hooks allow to add additional logic executed during transaction lifetime.
type Hooks interface {
	// OnPreCommitUnsafe is executed before Commit of transactions.
	// The given transaction is already locked. TODO simfg confuse 这个注释好像存在问题，看代码好像这个并没有被锁
	/***
	消息被commit之前被执行
	*/
	OnPreCommitUnsafe(tx BatchTx)
}

type hooks struct {
	onPreCommitUnsafe HookFunc
}

func (h hooks) OnPreCommitUnsafe(tx BatchTx) {
	h.onPreCommitUnsafe(tx)
}

func NewHooks(onPreCommitUnsafe HookFunc) Hooks {
	return hooks{onPreCommitUnsafe: onPreCommitUnsafe}
}
