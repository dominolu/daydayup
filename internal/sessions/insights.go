package sessions

// Timing describes gateway observations, never estimates from body length.
type Timing struct {
	Streaming        bool     `json:"streaming"`
	TTFTMS           *float64 `json:"ttft_ms"`
	GenerationMS     *float64 `json:"generation_ms"`
	GenerationTokens int64    `json:"generation_tokens"`
}

type Performance struct {
	ModelTimeMS      *float64 `json:"model_time_ms"`
	TTFTMS           *float64 `json:"ttft_ms"`
	TTFTSamples      int      `json:"ttft_samples"`
	TPS              *float64 `json:"tps"`
	TPSSamples       int      `json:"tps_samples"`
	GenerationMS     float64  `json:"generation_ms"`
	GenerationTokens int64    `json:"generation_tokens"`
	ToolTimeMS       *float64 `json:"tool_time_ms"`
	ToolCalls        int      `json:"tool_calls"`
	ToolTimeSource   string   `json:"tool_time_source,omitempty"`
}

type TokenDetails struct {
	UncachedInput        *int64   `json:"uncached_input_tokens"`
	CacheHitRate         *float64 `json:"cache_hit_rate"`
	CacheSamples         int      `json:"cache_samples"`
	CacheUnknownAttempts int      `json:"cache_unknown_attempts"`
	CacheObservedInput   int64    `json:"cache_observed_input_tokens"`
	CacheObservedRead    int64    `json:"cache_observed_read_tokens"`
}

func floatPointer(v float64) *float64 { return &v }
func intPointer(v int64) *int64       { return &v }
func addFloat(dst **float64, value float64) {
	if *dst == nil {
		*dst = floatPointer(0)
	}
	**dst += value
}

func decorate(r Record, tools map[string]ToolEvent) Record {
	r.Performance = Performance{}
	r.TokenDetails = TokenDetails{}
	var ttftSum float64
	for _, a := range r.Attempts {
		addFloat(&r.Performance.ModelTimeMS, float64(a.DurationMS))
		if a.Timing != nil {
			if a.Timing.TTFTMS != nil && *a.Timing.TTFTMS >= 0 {
				ttftSum += *a.Timing.TTFTMS
				r.Performance.TTFTSamples++
			}
			if a.Timing.GenerationMS != nil && *a.Timing.GenerationMS > 0 && a.Timing.GenerationTokens > 0 {
				r.Performance.GenerationMS += *a.Timing.GenerationMS
				r.Performance.GenerationTokens += a.Timing.GenerationTokens
				r.Performance.TPSSamples++
			}
		}
		u := a.Usage
		// Old records with no field provenance cannot prove cache coverage.
		if u.Known && hasField(u.Reported, "input_tokens") && hasField(u.Reported, "cached_input_tokens") && u.Cached >= 0 && u.Input >= u.Cached {
			if r.TokenDetails.UncachedInput == nil {
				r.TokenDetails.UncachedInput = intPointer(0)
			}
			*r.TokenDetails.UncachedInput += u.Input - u.Cached
			r.TokenDetails.CacheObservedInput += u.Input
			r.TokenDetails.CacheObservedRead += u.Cached
			r.TokenDetails.CacheSamples++
		} else {
			r.TokenDetails.CacheUnknownAttempts++
		}
	}
	if r.Performance.TTFTSamples > 0 {
		r.Performance.TTFTMS = floatPointer(ttftSum / float64(r.Performance.TTFTSamples))
	}
	if r.Performance.GenerationMS > 0 {
		r.Performance.TPS = floatPointer(float64(r.Performance.GenerationTokens) * 1000 / r.Performance.GenerationMS)
	}
	if r.TokenDetails.CacheObservedInput > 0 {
		r.TokenDetails.CacheHitRate = floatPointer(100 * float64(r.TokenDetails.CacheObservedRead) / float64(r.TokenDetails.CacheObservedInput))
	}
	for _, tool := range tools {
		if tool.RequestID == r.ID {
			addTool(&r.Performance, tool)
		}
	}
	return r
}

func addTool(p *Performance, tool ToolEvent) {
	addFloat(&p.ToolTimeMS, tool.DurationMS)
	p.ToolCalls++
	p.ToolTimeSource = "client_reported"
}

func mergeInsights(summary *Summary, r Record) {
	p, rp := &summary.Performance, r.Performance
	if rp.ModelTimeMS != nil {
		addFloat(&p.ModelTimeMS, *rp.ModelTimeMS)
	}
	if rp.TTFTMS != nil {
		sum := *rp.TTFTMS * float64(rp.TTFTSamples)
		if p.TTFTMS != nil {
			sum += *p.TTFTMS * float64(p.TTFTSamples)
		}
		p.TTFTSamples += rp.TTFTSamples
		p.TTFTMS = floatPointer(sum / float64(p.TTFTSamples))
	}
	p.GenerationMS += rp.GenerationMS
	p.GenerationTokens += rp.GenerationTokens
	p.TPSSamples += rp.TPSSamples
	if p.GenerationMS > 0 {
		p.TPS = floatPointer(float64(p.GenerationTokens) * 1000 / p.GenerationMS)
	}
	d, rd := &summary.TokenDetails, r.TokenDetails
	if rd.UncachedInput != nil {
		if d.UncachedInput == nil {
			d.UncachedInput = intPointer(0)
		}
		*d.UncachedInput += *rd.UncachedInput
	}
	d.CacheSamples += rd.CacheSamples
	d.CacheUnknownAttempts += rd.CacheUnknownAttempts
	d.CacheObservedInput += rd.CacheObservedInput
	d.CacheObservedRead += rd.CacheObservedRead
	if d.CacheObservedInput > 0 {
		d.CacheHitRate = floatPointer(100 * float64(d.CacheObservedRead) / float64(d.CacheObservedInput))
	}
}
