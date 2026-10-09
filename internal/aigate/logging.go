package aigate

func (g *Gateway) logDebug(format string, args ...any) { g.log.Debugf(format, args...) }
func (g *Gateway) logInfo(format string, args ...any)  { g.log.Infof(format, args...) }
func (g *Gateway) logError(format string, args ...any) { g.log.Errorf(format, args...) }
