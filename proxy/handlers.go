package proxy

import (
	"regexp"
	"strings"

	"github.com/sammy007/open-ethereum-pool/rpc"
	. "github.com/sammy007/open-ethereum-pool/util"
)

// Allow only lowercase hexadecimal with 0x prefix
var noncePattern = regexp.MustCompile("^0x[0-9a-f]{16}$")
var hashPattern = regexp.MustCompile("^0x[0-9a-f]{64}$")
var workerPattern = regexp.MustCompile("^[0-9a-zA-Z-_]{1,64}$")

// Stratum
func (s *ProxyServer) handleLoginRPC(cs *Session, params []string, id string) (bool, *ErrorReply) {
	if len(params) == 0 {
		return false, &ErrorReply{Code: -1, Message: "Invalid params"}
	}

	login := strings.ToLower(params[0])
	if !IsValidHexAddress(login) {
		return false, &ErrorReply{Code: -1, Message: "Invalid login"}
	}
	if !s.policy.ApplyLoginPolicy(login, cs.ip) {
		return false, &ErrorReply{Code: -1, Message: "You are blacklisted"}
	}
	cs.login = login
	cs.id = id
	cs.diff = s.diff
	s.registerSession(cs)
	Info.Printf("Stratum miner connected %v.%v@%v", login, cs.id, cs.ip)
	return true, nil
}

func (s *ProxyServer) handleGetWorkRPC(cs *Session) ([]string, *ErrorReply) {
	t := s.currentBlockTemplate()
	if t == nil || len(t.Header) == 0 || s.isSick() {
		return nil, &ErrorReply{Code: 0, Message: "Work not ready"}
	}
	return []string{t.Header, t.Seed, cs.diff}, nil
}

// Stratum
func (s *ProxyServer) handleTCPSubmitRPC(cs *Session, id string, params []string) (bool, *ErrorReply) {
	s.sessionsMu.RLock()
	_, ok := s.sessions[cs]
	s.sessionsMu.RUnlock()

	if !ok {
		return false, &ErrorReply{Code: 25, Message: "Not subscribed"}
	}
	return s.handleSubmitRPC(cs, cs.login, id, params)
}

func (s *ProxyServer) handleSubmitRPC(cs *Session, login, id string, params []string) (bool, *ErrorReply) {
	if !workerPattern.MatchString(id) {
		id = "0"
	}
	if len(params) != 3 {
		s.policy.ApplyMalformedPolicy(cs.ip)
		Error.Printf("Malformed params from %s@%s %v", login, cs.ip, params)
		return false, &ErrorReply{Code: -1, Message: "Invalid params"}
	}

	if !noncePattern.MatchString(params[0]) || !hashPattern.MatchString(params[1]) || !hashPattern.MatchString(params[2]) {
		s.policy.ApplyMalformedPolicy(cs.ip)
		Error.Printf("Malformed PoW result from %s@%s %v", login, cs.ip, params)
		return false, &ErrorReply{Code: -1, Message: "Malformed PoW result"}
	}
	t := s.currentBlockTemplate()
	exist, validShare := s.processShare(login, id, cs.ip, TargetHexToDiff(cs.diff).Int64(), t, params)
	ok := s.policy.ApplySharePolicy(cs.ip, !exist && validShare)

	if exist {
		Error.Printf("Duplicate share from %s@%s %v", login, cs.ip, params)
		ShareLog.Printf("Duplicate share from %s@%s %v", login, cs.ip, params)
		return false, &ErrorReply{Code: 22, Message: "Duplicate share"}
	}

	if !validShare {
		Error.Printf("Invalid share from %s.%s@%s", login, id, cs.ip)
		ShareLog.Printf("Invalid share from %s.%s@%s", login, id, cs.ip)
		// Bad shares limit reached, return error and close
		if !ok {
			return false, &ErrorReply{Code: 23, Message: "Invalid share"}
		}
		return false, nil
	}
	Info.Printf("Valid share from %s.%s@%s", login, id, cs.ip)
	ShareLog.Printf("Valid share from %s.%s@%s", login, id, cs.ip)

	if !ok {
		return true, &ErrorReply{Code: -1, Message: "High rate of invalid shares"}
	}

	if s.config.Proxy.DynamicDifficulty.Enabled && cs.lastShareTime != 0 {
		roundDuration := MakeTimestamp() - cs.lastShareTime

		// 计算当前会话的share提交频率
		submitRate := (60 * 1000) / roundDuration

		// 抑制过快的提交
		var factor float64
		diff := TargetHexToDiff(cs.diff).Int64()
		switch {
		case submitRate > s.config.Proxy.DynamicDifficulty.MaxSubmitRate:
			factor = 1.2
		case submitRate < s.config.Proxy.DynamicDifficulty.MinSubmitRate:
			factor = 0.8
		default:
			factor = 1.0
		}
		diff = int64(float64(diff) * factor)
		cs.diff = GetTargetHex(diff)
		Info.Printf("Diff adjust to %d in %s.%s(%s)", diff, login[0:10], id, cs.ip)

		// 下发到矿机
		reply := []string{t.Header, t.Seed, cs.diff}
		err := cs.pushNewJob(&reply)
		if err != nil {
			Error.Printf("Job transmit error to %v.%v@%v: %v", cs.login, id, cs.ip, err)
		} else {
			Error.Printf("Send new job to stratum miner: %v.%v@%v", cs.login, id, cs.ip)
		}
	}

	// 记录提交时间戳
	cs.lastShareTime = MakeTimestamp()

	return true, nil
}

func (s *ProxyServer) handleGetBlockByNumberRPC() *rpc.GetBlockReplyPart {
	t := s.currentBlockTemplate()
	var reply *rpc.GetBlockReplyPart
	if t != nil {
		reply = t.GetPendingBlockCache
	}
	return reply
}

func (s *ProxyServer) handleUnknownRPC(cs *Session, m string) *ErrorReply {
	Error.Printf("Unknown request method %s from %s", m, cs.ip)
	s.policy.ApplyMalformedPolicy(cs.ip)
	return &ErrorReply{Code: -3, Message: "Method not found"}
}