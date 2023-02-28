// Copyright 2020 The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package exporter

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/prometheus-community/json_exporter/config"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/promlog"
	"k8s.io/client-go/util/jsonpath"
)

type JSONMetricCollector struct {
	JSONMetrics []JSONMetric
	Data        []byte
	Logger      log.Logger
}

type JSONMetric struct {
	Desc                   *prometheus.Desc
	Type                   config.ScrapeType
	KeyJSONPath            string
	ValueJSONPath          string
	LabelsJSONPaths        []string
	ValueType              prometheus.ValueType
	ValueConverter         config.ValueConverterType
	EpochTimestampJSONPath string
}

var jsonExporterStatusDesc *prometheus.Desc = prometheus.NewDesc("json_exporter_status", "Up/Down Status of JSON Exporter. Should always be 0.", nil, nil)

func (mc JSONMetricCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- jsonExporterStatusDesc

	for _, m := range mc.JSONMetrics {
		ch <- m.Desc
	}
}

func (mc JSONMetricCollector) Collect(ch chan<- prometheus.Metric) {
	var rootData []byte = mc.Data

	ch <- prometheus.MustNewConstMetric(
		jsonExporterStatusDesc,
		prometheus.GaugeValue,
		0,
	)

	for _, m := range mc.JSONMetrics {
		switch m.Type {
		case config.ValueScrape:
			level.Info(mc.Logger).Log("msg", "Extracting value via ValueScrape for: "+m.KeyJSONPath)
			value, err := extractValue(mc.Logger, mc.Data, m.KeyJSONPath, false)
			if err != nil {
				level.Error(mc.Logger).Log("msg", "Failed to extract value for metric", "path", m.KeyJSONPath, "err", err, "metric", m.Desc)
				continue
			}

			if floatValue, err := SanitizeValue(value); err == nil {
				ch <- prometheus.MustNewConstMetric(
					m.Desc,
					m.ValueType,
					floatValue,
					extractLabels(mc.Logger, mc.Data, m.LabelsJSONPaths)...,
				)
			} else {
				level.Error(mc.Logger).Log("msg", "Failed to convert extracted value to float64", "path", m.KeyJSONPath, "value", value, "err", err, "metric", m.Desc)
				continue
			}

		case config.ObjectScrape:
			level.Info(mc.Logger).Log("msg", "Extracting value via Object for: "+m.KeyJSONPath)

			values, err := extractValue(mc.Logger, mc.Data, m.KeyJSONPath, true)

			if err != nil {
				level.Error(mc.Logger).Log("msg", "Failed to extract json objects for metric", "err", err, "metric", m.Desc)
				continue
			}

			level.Debug(mc.Logger).Log("msg", "mc.Data: "+string(mc.Data))
			level.Debug(mc.Logger).Log("msg", "extracted values: "+string(values))

			level.Info(mc.Logger).Log("msg", "Extracted value for "+m.KeyJSONPath+". Going to loop through array if present")

			var jsonData []interface{}
			if err := json.Unmarshal([]byte(values), &jsonData); err == nil {
				for _, data := range jsonData {
					jdata, err := json.Marshal(data)
					if err != nil {
						level.Error(mc.Logger).Log("msg", "Failed to marshal data to json", "path", m.ValueJSONPath, "err", err, "metric", m.Desc, "data", data)
						continue
					}
					level.Info(mc.Logger).Log("msg", "Extracting value for JSON element in array using thie ValueJSONPath of "+m.ValueJSONPath)
					level.Debug(mc.Logger).Log("msg", "jdata: "+string(jdata))
					value, err := extractValue(mc.Logger, jdata, m.ValueJSONPath, false)

					if err != nil {
						level.Error(mc.Logger).Log("msg", "Failed to extract value for metric", "path", m.ValueJSONPath, "err", err, "metric", m.Desc)
						continue
					}

					value = convertValueIfNeeded(m, value)
					level.Debug(mc.Logger).Log("msg", "Value for "+m.KeyJSONPath+" is "+value)

					// Choose what jdata to insert into extraction of label
					if floatValue, err := SanitizeValue(value); err == nil {
						ch <- prometheus.MustNewConstMetric(
							m.Desc,
							m.ValueType,
							floatValue,
							extractLabelsWithParentNode(mc.Logger, jdata, rootData, m.LabelsJSONPaths)...,
						)
					} else {
						level.Error(mc.Logger).Log("msg", "Failed to convert extracted value to float64", "path", m.ValueJSONPath, "value", value, "err", err, "metric", m.Desc)
						continue
					}
				}
			} else {
				level.Error(mc.Logger).Log("msg", "Failed to convert extracted objects to json", "err", err, "metric", m.Desc)
				continue
			}
		default:
			level.Error(mc.Logger).Log("msg", "Unknown scrape config type", "type", m.Type, "metric", m.Desc)
			continue
		}
	}
}

var promlogConfig *promlog.Config = &promlog.Config{}
var collectorLogger log.Logger = promlog.New(promlogConfig)

// Returns the last matching value at the given json path
func extractValue(logger log.Logger, data []byte, path string, enableJSONOutput bool) (string, error) {
	var jsonData interface{}
	buf := new(bytes.Buffer)

	j := jsonpath.New("jp")
	if enableJSONOutput {
		j.EnableJSONOutput(true)
	}

	if err := json.Unmarshal(data, &jsonData); err != nil {
		level.Error(logger).Log("msg", "Failed to unmarshal data to json", "err", err, "data", data)
		return "", err
	}

	level.Debug(logger).Log("msg", "jsonData: "+fmt.Sprintf("%v", jsonData))

	if err := j.Parse(path); err != nil {
		level.Error(logger).Log("msg", "Failed to parse jsonpath", "err", err, "path", path, "data", data)
		return "", err
	}

	if err := j.Execute(buf, jsonData); err != nil {
		level.Error(logger).Log("msg", "Failed to execute jsonpath", "err", err, "path", path, "data", data)
		return "", err
	}

	level.Debug(logger).Log("msg", "buf.string(): "+buf.String())

	// Since we are finally going to extract only float64, unquote if necessary
	if res, err := jsonpath.UnquoteExtend(buf.String()); err == nil {
		return res, nil
	}

	return buf.String(), nil
}

// Returns the list of labels created from the list of provided json paths
func extractLabels(logger log.Logger, data []byte, paths []string) []string {
	labels := make([]string, len(paths))
	for i, path := range paths {
		if result, err := extractValue(logger, data, path, false); err == nil {
			labels[i] = result
		} else {
			level.Error(logger).Log("msg", "Failed to extract label value", "err", err, "path", path, "data", data)
		}
	}
	return labels
}

func extractLabelsWithParentNode(logger log.Logger, childData []byte, rootData []byte, paths []string) []string {
	labels := make([]string, len(paths))
	for i, path := range paths {
		var selectedData []byte = selectRightJsonData(rootData, childData, path)
		if result, err := extractValue(logger, selectedData, path, false); err == nil {
			labels[i] = result
		} else {
			level.Error(logger).Log("msg", "Failed to extract label value", "err", err, "path", path, "data", childData)
		}
	}
	return labels
}

// Returns the conversion of the dynamic value- if it exists in the ValueConverter configuration
func convertValueIfNeeded(m JSONMetric, value string) string {
	if m.ValueConverter != nil {
		if valueMappings, hasPathKey := m.ValueConverter[m.ValueJSONPath]; hasPathKey {
			value = strings.ToLower(value)

			if _, hasValueKey := valueMappings[value]; hasValueKey {
				value = valueMappings[value]
			}
		}
	}
	return value
}

func selectRightJsonData(rootData []byte, childData []byte, path string) []byte {
	var noSpacePath string = strings.ReplaceAll(path, " ", "")

	if strings.Contains(noSpacePath[0:4], "$") {
		level.Debug(collectorLogger).Log("msg", "Using JSON data from the root")
		return rootData
	} else {
		level.Debug(collectorLogger).Log("msg", "Using JSON data from the child nodes")
		return childData
	}
}
