package store

import (
	"strings"
	"time"

	"github.com/garyburd/redigo/redis"
)

const (
	redisCASMissingKey = "key does not exist"
	redisCASScript     = `
local v = redis.call('get', KEYS[1])
if v == false then
  return redis.error_reply("key does not exist")
end
if v ~= ARGV[1] then
  return 0
end
if ARGV[3] ~= "0" then
  redis.call('setex', KEYS[1], ARGV[3], ARGV[2])
else
  redis.call('set', KEYS[1], ARGV[2])
end
return 1
`
)

// RedisStore implements a Redis-based store.
type redisStore struct {
	pool         *redis.Pool
	prefix       string
	db           int
	supportsEval bool
}

// NewRedisStore creates a new Redis-based store, using the provided pool to get its
// connections. The keys will have the specified keyPrefix, which may be an empty string,
// and the database index specified by db will be selected to store the keys. Any
// updating operations will reset the key TTL to the provided value rounded down to
// the nearest second.
func NewRedisStore(pool *redis.Pool, keyPrefix string, db int) GCRAStore {
	return &redisStore{
		pool:         pool,
		prefix:       keyPrefix,
		db:           db,
		supportsEval: true,
	}
}

// Get returns the value of the key if it is in the Store or -1 if it does
// not exist.
func (r *redisStore) GetWithTime(key string) (int64, time.Time, error) {
	var now time.Time

	key = r.prefix + key

	conn, err := r.getConn()
	if err != nil {
		return 0, now, err
	}
	defer conn.Close()

	conn.Send("TIME")
	conn.Send("GET", key)
	conn.Flush()
	timeReply, err := redis.Values(conn.Receive())
	if err != nil {
		return 0, now, err
	}

	var s, ms int64
	if _, err := redis.Scan(timeReply, &s, &ms); err != nil {
		return 0, now, err
	}
	now = time.Unix(s, ms*int64(time.Millisecond))

	v, err := redis.Int64(conn.Receive())
	if err == redis.ErrNil {
		return -1, now, nil
	} else if err != nil {
		return 0, now, err
	}

	return v, now, nil
}

// SetIfNotExists sets the value of key only if it is not already set in the Store
// it returns whether a new value was set.
func (r *redisStore) SetIfNotExists(key string, value int64, ttl time.Duration) (bool, error) {
	key = r.prefix + key

	conn, err := r.getConn()
	if err != nil {
		return false, err
	}
	defer conn.Close()

	v, err := redis.Int64(conn.Do("SETNX", key, value))
	if err != nil {
		return false, err
	}

	updated := v == 1

	if ttl >= time.Second {
		if _, err := conn.Do("EXPIRE", key, int(ttl.Seconds())); err != nil {
			return updated, err
		}
	}

	return updated, nil
}

// CompareAndSwap atomically compares the value at key to the old value.
// If it matches, it sets it to the new value and returns true. Otherwise,
// it returns false. If the key does not exist in the store, it returns
// false with no error.
func (r *redisStore) CompareAndSwap(key string, old, new int64, ttl time.Duration) (bool, error) {
	key = r.prefix + key
	conn, err := r.getConn()
	if err != nil {
		return false, err
	}
	defer conn.Close()

	if r.supportsEval {
		swapped, err := r.compareAndSwapWithEval(conn, key, old, new, ttl)
		if err == nil {
			return swapped, nil
		}

		// If failure is due to EVAL being unsupported, note that and
		// retry using WATCH
		if strings.Contains(err.Error(), "unknown command") {
			r.supportsEval = false
		} else {
			return false, err
		}
	}

	swapped, err := r.compareAndSwapWithWatch(conn, key, old, new, ttl)
	if err != nil {
		return false, err
	}

	return swapped, nil
}

func (r *redisStore) compareAndSwapWithWatch(conn redis.Conn, key string, old, new int64, ttl time.Duration) (bool, error) {
	conn.Send("WATCH", key)
	conn.Send("GET", key)
	conn.Flush()
	conn.Receive()

	v, err := redis.Int64(conn.Receive())
	if err == redis.ErrNil {
		return false, nil
	}
	if v != old {
		return false, nil
	}

	conn.Send("MULTI")
	if ttl > 0 {
		conn.Send("SETEX", key, int(ttl.Seconds()), new)
	} else {
		conn.Send("SET", key, new)
	}
	if _, err := conn.Do("EXEC"); err == redis.ErrNil {
		return false, nil
	} else if err != nil {
		return false, err
	}

	return true, nil
}

func (r *redisStore) compareAndSwapWithEval(conn redis.Conn, key string, old, new int64, ttl time.Duration) (bool, error) {
	swapped, err := redis.Bool(conn.Do("EVAL", redisCASScript, 1, key, old, new, int(ttl.Seconds())))
	if err != nil {
		if strings.Contains(err.Error(), redisCASMissingKey) {
			return false, nil
		}

		return false, err
	}

	return swapped, nil
}

// Select the specified database index.
func (r *redisStore) getConn() (redis.Conn, error) {
	conn := r.pool.Get()

	// Select the specified database
	if r.db > 0 {
		if _, err := redis.String(conn.Do("SELECT", r.db)); err != nil {
			conn.Close()
			return nil, err
		}
	}

	return conn, nil
}
