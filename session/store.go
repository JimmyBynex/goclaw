package session

import (
	"bytes"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sync"
)

type Store interface {
	Get(key SessionKey) (*Session, error)
	Save(sess *Session) error
	Delete(key SessionKey) error
}

type FileStore struct {
	dir   string
	mu    sync.RWMutex
	cache map[string]*Session
}

func NewFileStore(dir string) (*FileStore, error) {
	//unix的mkdir -p递归创建文件夹，可以查询参数的含义
	return &FileStore{
		dir:   dir,
		cache: make(map[string]*Session),
	}, os.MkdirAll(dir, 0700)
}

func (s *FileStore) Get(key SessionKey) (*Session, error) {

	id := key.String()
	s.mu.RLock()
	sees := s.cache[id]
	if sees != nil {
		s.mu.RUnlock()
		return sees, nil
	}
	s.mu.RUnlock()

	s.mu.Lock()
	defer s.mu.Unlock()

	//防止两个goroutine做无效功用，一个已经把cache重新存入，另一个就不用再重复读取了
	if sess := s.cache[id]; sess != nil {
		return sess, nil
	}

	res, err := os.ReadFile(filepath.Join(s.dir, id+".json"))
	//第一种情况是没有文件，返回空sess
	if os.IsNotExist(err) {
		sess := New(key)
		s.cache[id] = sess
		return sess, nil
	}
	//第二种情况是真的出错了，处理错误
	if err != nil {
		log.Printf("[session]Error reading file %s: %v", id, err)
		return nil, err
	}
	//第三种就是读出存有的文件
	var sess Session
	json.NewDecoder(bytes.NewBuffer(res)).Decode(&sess)
	s.cache[id] = &sess
	return &sess, nil
}

func (s *FileStore) Save(sess *Session) error {
	id := sess.Key.String()

	s.mu.Lock()
	s.cache[id] = sess
	s.mu.Unlock()
	data, err := json.Marshal(sess)
	if err != nil {
		log.Printf("[session]Error marshalling session: %v", err)
		return err
	}
	//为什么需要先写到tmp再rename，保持原子操作，防止程序崩溃
	tmp := filepath.Join(s.dir, id+".json.tmp")
	err = os.WriteFile(tmp, data, 0600)
	if err != nil {
		log.Printf("[session]Error writing file: %v", err)
		return err
	}
	err = os.Rename(tmp, filepath.Join(s.dir, id+".json"))
	if err != nil {
		log.Printf("[session]Error renaming file: %v", err)
		return err
	}
	return nil
}

func (s *FileStore) Delete(key SessionKey) error {
	id := key.String()
	s.mu.Lock()
	err := os.Remove(filepath.Join(s.dir, id+".json"))
	if err != nil {
		log.Printf("[session]Error deleting file: %v", err)
	}
	//不用s.cache[id] = nil，因为这样cache还在,直接删除引用触发gc
	delete(s.cache, id)
	s.mu.Unlock()
	return nil
}
