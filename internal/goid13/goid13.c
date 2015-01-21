#include <runtime.h>

void ·GetGoID(int64 ret) {
  ret = g->goid;
  USED(&ret);
}
