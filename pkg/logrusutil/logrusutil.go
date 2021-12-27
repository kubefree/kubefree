package logrusutil

import "github.com/sirupsen/logrus"

type DefaultFieldsFormatter struct {
	WrappedFormatter logrus.Formatter
	DefaultFields    logrus.Fields
	PrintLineNumber  bool
}

// Format implements logrus.Formatter's Format. We allocate a new Fields
// map in order to not modify the caller's Entry, as that is not a thread
// safe operation.
func (f *DefaultFieldsFormatter) Format(entry *logrus.Entry) ([]byte, error) {
	data := make(logrus.Fields, len(entry.Data)+len(f.DefaultFields))

	for k, v := range f.DefaultFields {
		data[k] = v
	}
	for k, v := range entry.Data {
		data[k] = v
	}
	return f.WrappedFormatter.Format(&logrus.Entry{
		Logger:  entry.Logger,
		Data:    data,
		Time:    entry.Time,
		Level:   entry.Level,
		Message: entry.Message,
		Caller:  entry.Caller,
	})
}

func Init(formatter *DefaultFieldsFormatter) {
	if formatter == nil {
		return
	}

	if formatter.WrappedFormatter == nil {
		formatter.WrappedFormatter = &logrus.JSONFormatter{}
	}
	logrus.SetFormatter(formatter)
	logrus.SetReportCaller(formatter.PrintLineNumber)
}

func ComponentInit(name string) {
	Init(&DefaultFieldsFormatter{
		PrintLineNumber: true,
		DefaultFields:   logrus.Fields{"component": name},
	})
}
