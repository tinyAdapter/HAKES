/*
 * Copyright 2024 The HAKES Authors
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

#include <sys/time.h>

#include <cstdio>

#include "Enclave_u.h"

void ocall_printf(const char *str) { printf("%s", str); }

// print in microseconds time since epoch
void ocall_print_time() {
  struct timeval t;
  gettimeofday(&t, 0);
  printf("timing since start: %llu\n",
         (unsigned long long)t.tv_sec * 1000 * 1000 +
             (unsigned long long)t.tv_usec);
}