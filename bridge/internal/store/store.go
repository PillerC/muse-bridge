package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

// Store 持有唯一的 SQLite 连接。
//
// 连接数固定为 1：SQLite 的写是串行的，接两个连接只会换来 "database is locked"
// 和重试逻辑。桥的吞吐是每秒几条消息，单连接远不是瓶颈，却能让事务语义变简单。
type Store struct {
	db *sql.DB
}

// Open 打开数据库、启用 WAL 并执行建表。
func Open(path string) (*Store, error) {
	dsn := "file:" + path +
		"?_pragma=journal_mode(WAL)" +
		"&_pragma=busy_timeout(5000)" +
		"&_pragma=foreign_keys(1)" +
		"&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("打开数据库失败: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetConnMaxLifetime(0)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("连接数据库失败: %w", err)
	}
	if _, err := db.ExecContext(ctx, schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("建表失败: %w", err)
	}
	return &Store{db: db}, nil
}

// Close 关闭连接。
func (s *Store) Close() error { return s.db.Close() }

// DB 暴露底层连接，供少量跨领域查询使用。
func (s *Store) DB() *sql.DB { return s.db }

// Tx 在事务中执行 fn。fn 返回错误则回滚。
func (s *Store) Tx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}
