// Package cache is the gateway's response cache: an exact layer keyed on a
// canonical hash of the request, and a semantic layer that serves a
// stored answer to a reworded last user turn when everything else about
// the request (tenant, model, system prompt, earlier turns, sampling
// parameters) is identical.
package cache
