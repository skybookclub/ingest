package main

import "testing"

func TestConvertISBN10to13(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{
			name:    "valid conversion",
			input:   "0747532699",
			want:    "9780747532699",
			wantErr: false,
		},
		{
			name:    "valid conversion with X",
			input:   "155404295X",
			want:    "9781554042951",
			wantErr: false,
		},
		{
			name:    "invalid length",
			input:   "12345",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := convertISBN10to13(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("convertISBN10to13() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if got != tt.want && !tt.wantErr {
				t.Errorf("convertISBN10to13() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestConvertISBN13to10(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{
			name:    "valid conversion",
			input:   "9780747532699",
			want:    "0747532699",
			wantErr: false,
		},
		{
			name:    "valid conversion resulting in X",
			input:   "9781554042951",
			want:    "155404295X",
			wantErr: false,
		},
		{
			name:    "invalid prefix",
			input:   "9790747532699",
			wantErr: true,
		},
		{
			name:    "invalid length",
			input:   "97807475",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := convertISBN13to10(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("convertISBN13to10() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if got != tt.want && !tt.wantErr {
				t.Errorf("convertISBN13to10() = %v, want %v", got, tt.want)
			}
		})
	}
}
