// Package config 加载 YAML 配置 + 环境变量覆盖（technical-design §1/§9）。
package config

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/dddpeter/agent-nexus/internal/adapter"
)

// Config 全量配置结构（config.yaml）。
type Config struct {
	Server struct {
		Listen    string `yaml:"listen"`
		AuthToken string `yaml:"auth_token"`
		DataDir   string `yaml:"data_dir"`
	} `yaml:"server"`

	Store struct {
		Driver string `yaml:"driver"`
		DSN    string `yaml:"dsn"`
	} `yaml:"store"`

	Orchestration struct {
		Engine   string `yaml:"engine"` // temporal | builtin
		Temporal struct {
			Host      string `yaml:"host"`
			Namespace string `yaml:"namespace"`
		} `yaml:"temporal"`
	} `yaml:"orchestration"`

	Registry struct {
		HeartbeatInterval   adapter.Duration `yaml:"heartbeat_interval"`
		OfflineThreshold    int              `yaml:"offline_threshold"`
		DefaultChildTimeout adapter.Duration `yaml:"default_child_timeout"`
		MaxChildren         int              `yaml:"max_children"`
		KillGrace           adapter.Duration `yaml:"kill_grace"`
		PgidFile            string           `yaml:"pgid_file"`
	} `yaml:"registry"`

	// Agents 启动时批量载入的静态注册（FR1.1 配置文件途径）。
	Agents []AgentStatic `yaml:"agents"`
}

// AgentStatic 配置文件中的 agent 注册项。
type AgentStatic struct {
	Name         string                `yaml:"name"`
	Adapter      string                `yaml:"adapter"`
	Config       map[string]any        `yaml:"config"`
	Capabilities *adapter.Capabilities `yaml:"capabilities,omitempty"`
}

// Default 返回默认配置。
func Default() Config {
	var c Config
	c.Server.Listen = ":8080"
	c.Server.DataDir = "data"
	c.Store.Driver = "sqlite"
	c.Store.DSN = "data/nexus.db"
	c.Orchestration.Engine = "builtin" // 一期默认内置降级调度器（Temporal 未接入时）
	c.Registry.HeartbeatInterval = adapter.Duration(15e9)
	c.Registry.OfflineThreshold = 3
	c.Registry.DefaultChildTimeout = adapter.Duration(1800e9)
	c.Registry.MaxChildren = 32
	c.Registry.KillGrace = adapter.Duration(5e9)
	return c
}

// Load 读取 YAML 配置文件并叠加默认值；文件不存在时返回默认值（可选启动）。
// 标量中的 ${ENV_VAR} 引用统一解析（含 auth_token）。
func Load(path string) (Config, error) {
	c := Default()
	if path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				return c, nil
			}
			return c, fmt.Errorf("config: read %s: %w", path, err)
		}
		if err := yaml.Unmarshal(raw, &c); err != nil {
			return c, fmt.Errorf("config: parse %s: %w", path, err)
		}
	}
	c.applyDefaults()
	c.resolveEnvRefs()
	return c, nil
}

func (c *Config) applyDefaults() {
	d := Default()
	if c.Server.Listen == "" {
		c.Server.Listen = d.Server.Listen
	}
	if c.Server.DataDir == "" {
		c.Server.DataDir = d.Server.DataDir
	}
	if c.Store.Driver == "" {
		c.Store.Driver = d.Store.Driver
	}
	if c.Store.DSN == "" {
		c.Store.DSN = d.Store.DSN
	}
	if c.Orchestration.Engine == "" {
		c.Orchestration.Engine = d.Orchestration.Engine
	}
	if c.Registry.HeartbeatInterval <= 0 {
		c.Registry.HeartbeatInterval = d.Registry.HeartbeatInterval
	}
	if c.Registry.OfflineThreshold <= 0 {
		c.Registry.OfflineThreshold = d.Registry.OfflineThreshold
	}
	if c.Registry.DefaultChildTimeout < 0 {
		c.Registry.DefaultChildTimeout = d.Registry.DefaultChildTimeout
	}
	if c.Registry.MaxChildren <= 0 {
		c.Registry.MaxChildren = d.Registry.MaxChildren
	}
	if c.Registry.KillGrace <= 0 {
		c.Registry.KillGrace = d.Registry.KillGrace
	}
	if c.Registry.PgidFile == "" {
		c.Registry.PgidFile = c.Server.DataDir + "/pgids.json"
	}
}

// resolveEnvRefs 解析标量中的 ${ENV_VAR} 引用（auth_token 等密钥不入库文件）。
func (c *Config) resolveEnvRefs() {
	c.Server.AuthToken = resolveEnv(c.Server.AuthToken)
	c.Store.DSN = resolveEnv(c.Store.DSN)
	for i := range c.Agents {
		for k, v := range c.Agents[i].Config {
			if s, ok := v.(string); ok {
				c.Agents[i].Config[k] = resolveEnv(s)
			}
		}
	}
}

func resolveEnv(s string) string {
	if strings.HasPrefix(s, "${") && strings.HasSuffix(s, "}") {
		return os.Getenv(strings.TrimSuffix(strings.TrimPrefix(s, "${"), "}"))
	}
	return s
}
