// Copyright 2016 The etcd Authors
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

package auth

// CAUTION: This random number based token mechanism is only for testing purpose.
// JWT based mechanism will be added in the near future.

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
)

const (
	letters                  = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"
	defaultSimpleTokenLength = 16
)

// var for testing purposes
// TODO: Remove this mutable global state - as it's race-prone.
var (
	simpleTokenTTLDefault    = 300 * time.Second
	simpleTokenTTLResolution = 1 * time.Second
)

/***
token存储者
*/
type simpleTokenTTLKeeper struct {
	tokens          map[string]time.Time
	donec           chan struct{}
	stopc           chan struct{}
	deleteTokenFunc func(string)
	mu              *sync.Mutex
	simpleTokenTTL  time.Duration
}

func (tm *simpleTokenTTLKeeper) stop() {
	select {
	case tm.stopc <- struct{}{}:
	case <-tm.donec:
	}
	<-tm.donec
}

/***
存储token
*/
func (tm *simpleTokenTTLKeeper) addSimpleToken(token string) {
	tm.tokens[token] = time.Now().Add(tm.simpleTokenTTL)
}

/***
重置token存储时间
*/
func (tm *simpleTokenTTLKeeper) resetSimpleToken(token string) {
	if _, ok := tm.tokens[token]; ok {
		tm.tokens[token] = time.Now().Add(tm.simpleTokenTTL)
	}
}

/***
删除token
*/
func (tm *simpleTokenTTLKeeper) deleteSimpleToken(token string) {
	delete(tm.tokens, token)
}

/***
每间隔1s，验证token是否失效，失效了则删除
*/
func (tm *simpleTokenTTLKeeper) run() {
	tokenTicker := time.NewTicker(simpleTokenTTLResolution)
	defer func() {
		tokenTicker.Stop()
		close(tm.donec)
	}()
	for {
		select {
		case <-tokenTicker.C:
			nowtime := time.Now()
			tm.mu.Lock()
			for t, tokenendtime := range tm.tokens {
				if nowtime.After(tokenendtime) {
					tm.deleteTokenFunc(t)
					delete(tm.tokens, t)
				}
			}
			tm.mu.Unlock()
		case <-tm.stopc:
			return
		}
	}
}

type tokenSimple struct {
	lg                *zap.Logger
	indexWaiter       func(uint64) <-chan struct{}
	simpleTokenKeeper *simpleTokenTTLKeeper
	simpleTokensMu    sync.Mutex
	simpleTokens      map[string]string // token -> username
	simpleTokenTTL    time.Duration
}

/***
随机获取一个token
*/
func (t *tokenSimple) genTokenPrefix() (string, error) {
	ret := make([]byte, defaultSimpleTokenLength)

	for i := 0; i < defaultSimpleTokenLength; i++ {
		bInt, err := rand.Int(rand.Reader, big.NewInt(int64(len(letters))))
		if err != nil {
			return "", err
		}

		ret[i] = letters[bInt.Int64()]
	}

	return string(ret), nil
}

/***
user与token进行保存
*/
func (t *tokenSimple) assignSimpleTokenToUser(username, token string) {
	t.simpleTokensMu.Lock()
	defer t.simpleTokensMu.Unlock()
	if t.simpleTokenKeeper == nil {
		return
	}

	_, ok := t.simpleTokens[token]
	if ok {
		t.lg.Panic(
			"failed to assign already-used simple token to a user",
			zap.String("user-name", username),
			zap.String("token", token),
		)
	}

	t.simpleTokens[token] = username
	t.simpleTokenKeeper.addSimpleToken(token)
}

/***
删除username对应的token
*/
func (t *tokenSimple) invalidateUser(username string) {
	if t.simpleTokenKeeper == nil {
		return
	}
	t.simpleTokensMu.Lock()
	for token, name := range t.simpleTokens {
		if name == username {
			delete(t.simpleTokens, token)
			t.simpleTokenKeeper.deleteSimpleToken(token)
		}
	}
	t.simpleTokensMu.Unlock()
}

/***
启用token存储者，使得token一定时间会自动过期
*/
func (t *tokenSimple) enable() {
	t.simpleTokensMu.Lock()
	defer t.simpleTokensMu.Unlock()
	if t.simpleTokenKeeper != nil { // already enabled
		return
	}
	if t.simpleTokenTTL <= 0 {
		t.simpleTokenTTL = simpleTokenTTLDefault
	}

	delf := func(tk string) {
		if username, ok := t.simpleTokens[tk]; ok {
			t.lg.Info(
				"deleted a simple token",
				zap.String("user-name", username),
				zap.String("token", tk),
			)
			delete(t.simpleTokens, tk)
		}
	}
	t.simpleTokenKeeper = &simpleTokenTTLKeeper{
		tokens:          make(map[string]time.Time),
		donec:           make(chan struct{}),
		stopc:           make(chan struct{}),
		deleteTokenFunc: delf,
		mu:              &t.simpleTokensMu,
		simpleTokenTTL:  t.simpleTokenTTL,
	}
	go t.simpleTokenKeeper.run()
}

/***
停止token存储者，会将所有的token删除
*/
func (t *tokenSimple) disable() {
	t.simpleTokensMu.Lock()
	tk := t.simpleTokenKeeper
	t.simpleTokenKeeper = nil
	t.simpleTokens = make(map[string]string) // invalidate all tokens
	t.simpleTokensMu.Unlock()
	if tk != nil {
		tk.stop()
	}
}

/***
获取token信息，同时也会延长token的过期时间
*/
func (t *tokenSimple) info(ctx context.Context, token string, revision uint64) (*AuthInfo, bool) {
	if !t.isValidSimpleToken(ctx, token) {
		return nil, false
	}
	t.simpleTokensMu.Lock()
	username, ok := t.simpleTokens[token]
	if ok && t.simpleTokenKeeper != nil {
		t.simpleTokenKeeper.resetSimpleToken(token)
	}
	t.simpleTokensMu.Unlock()
	return &AuthInfo{Username: username, Revision: revision}, ok
}

/***
使用username生成token，然后进行保存，在返回
*/
func (t *tokenSimple) assign(ctx context.Context, username string, rev uint64) (string, error) {
	// rev isn't used in simple token, it is only used in JWT
	var index uint64
	var ok bool
	if index, ok = ctx.Value(AuthenticateParamIndex{}).(uint64); !ok {
		return "", errors.New("failed to assign")
	}
	simpleTokenPrefix := ctx.Value(AuthenticateParamSimpleTokenPrefix{}).(string)
	token := fmt.Sprintf("%s.%d", simpleTokenPrefix, index)
	t.assignSimpleTokenToUser(username, token)

	return token, nil
}

/***
判断token是否有效
*/
func (t *tokenSimple) isValidSimpleToken(ctx context.Context, token string) bool {
	splitted := strings.Split(token, ".")
	if len(splitted) != 2 {
		return false
	}
	index, err := strconv.ParseUint(splitted[1], 10, 0)
	if err != nil {
		return false
	}

	select {
	case <-t.indexWaiter(uint64(index)):
		return true
	case <-ctx.Done():
	}

	return false
}

func newTokenProviderSimple(lg *zap.Logger, indexWaiter func(uint64) <-chan struct{}, TokenTTL time.Duration) *tokenSimple {
	if lg == nil {
		lg = zap.NewNop()
	}
	return &tokenSimple{
		lg:             lg,
		simpleTokens:   make(map[string]string),
		indexWaiter:    indexWaiter,
		simpleTokenTTL: TokenTTL,
	}
}
