/*
   Copyright The containerd Authors.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package cache

// TestBitmap exposes bitmap internals for unit tests.
type TestBitmap struct {
	bm *bitmap
}

func OpenOrCreateBitmapForTest(path string, n int) (*TestBitmap, error) {
	bm, err := openOrCreateBitmap(path, n)
	if err != nil {
		return nil, err
	}
	return &TestBitmap{bm: bm}, nil
}

func (t *TestBitmap) IsSetForTest(i int) bool        { return t.bm.isSet(i) }
func (t *TestBitmap) SetForTest(i int)               { t.bm.set(i) }
func (t *TestBitmap) CloseForTest()                  { t.bm.close() }
func (t *TestBitmap) PersistWordForTest(path string, idx int) error {
	return t.bm.persistWord(path, idx)
}
