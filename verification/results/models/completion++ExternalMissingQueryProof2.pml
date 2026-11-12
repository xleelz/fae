// num_comm_params=0
// num_mand_comm_params=0
// num_opt_comm_params=0

// git_link=https://github.com//blob///home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/external.go#L2
typedef Chandef {
	chan sync = [0] of {bool};
	chan enq = [0] of {bool};
	chan deq = [0] of {bool,bool};
	chan sending = [0] of {bool};
	chan rcving = [0] of {bool};
	chan closing = [0] of {bool};
	int size = 0;
	int num_msgs = 0;
	bool closed = false;
}



init { 
	chan child_ExternalMissingQueryProof20 = [1] of {int}->
	run ExternalMissingQueryProof2(child_ExternalMissingQueryProof20)->
	child_ExternalMissingQueryProof20?0->
stop_process:skip
}

proctype ExternalMissingQueryProof2(chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	chan child_AnonymousExternalMissingQueryProof733 = [1] of {int}->
	chan child_AnonymousExternalMissingQueryProof632 = [1] of {int}->
	chan child_AnonymousExternalMissingQueryProof531 = [1] of {int}->
	chan child_AnonymousExternalMissingQueryProof430 = [1] of {int}->
	Chandef adopted_ch->
	Chandef receipt_ch->
	Chandef performed_ch->
	Chandef intent_ch->
	run sync_monitor(intent_ch)->
	run sync_monitor(performed_ch)->
	run sync_monitor(receipt_ch)->
	run sync_monitor(adopted_ch)->
	run AnonymousExternalMissingQueryProof43(intent_ch,performed_ch,receipt_ch,adopted_ch,child_AnonymousExternalMissingQueryProof430)->
	run receiver(child_AnonymousExternalMissingQueryProof430)->
	run AnonymousExternalMissingQueryProof53(receipt_ch,adopted_ch,intent_ch,performed_ch,child_AnonymousExternalMissingQueryProof531)->
	run receiver(child_AnonymousExternalMissingQueryProof531)->
	run AnonymousExternalMissingQueryProof63(intent_ch,performed_ch,receipt_ch,adopted_ch,child_AnonymousExternalMissingQueryProof632)->
	run receiver(child_AnonymousExternalMissingQueryProof632)->
	run AnonymousExternalMissingQueryProof73(adopted_ch,intent_ch,performed_ch,receipt_ch,child_AnonymousExternalMissingQueryProof733)->
	run receiver(child_AnonymousExternalMissingQueryProof733)->
	

	if /* Rcv	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/external.go:8:2 */
	:: adopted_ch.deq?state,num_msgs /* 	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/external.go:8:2 */->
	:: adopted_ch.sync?state /* 	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/external.go:8:2 */ -> 
		adopted_ch.rcving!false
	fi->
	stop_process: skip->
	child!0
}
proctype AnonymousExternalMissingQueryProof43(Chandef intent_ch;Chandef performed_ch;Chandef receipt_ch;Chandef adopted_ch;chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	

	if /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/external.go:4:12 */
	:: intent_ch.enq?ok -> 
		assert(ok) /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/external.go:4:12 */
	:: intent_ch.sync!false -> 
		intent_ch.sending?ok->
		assert(ok) /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/external.go:4:12 */
	fi->
	stop_process: skip->
	child!0
}
proctype AnonymousExternalMissingQueryProof53(Chandef receipt_ch;Chandef adopted_ch;Chandef intent_ch;Chandef performed_ch;chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	

	if /* Rcv	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/external.go:5:12 */
	:: intent_ch.deq?state,num_msgs /* 	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/external.go:5:12 */->
	:: intent_ch.sync?state /* 	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/external.go:5:12 */ -> 
		intent_ch.rcving!false
	fi->
	

	if /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/external.go:5:21 */
	:: performed_ch.enq?ok -> 
		assert(ok) /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/external.go:5:21 */
	:: performed_ch.sync!false -> 
		performed_ch.sending?ok->
		assert(ok) /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/external.go:5:21 */
	fi->
	stop_process: skip->
	child!0
}
proctype AnonymousExternalMissingQueryProof63(Chandef intent_ch;Chandef performed_ch;Chandef receipt_ch;Chandef adopted_ch;chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	

	if /* Rcv	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/external.go:6:12 */
	:: performed_ch.deq?state,num_msgs /* 	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/external.go:6:12 */->
	:: performed_ch.sync?state /* 	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/external.go:6:12 */ -> 
		performed_ch.rcving!false
	fi->
	stop_process: skip->
	child!0
}
proctype AnonymousExternalMissingQueryProof73(Chandef adopted_ch;Chandef intent_ch;Chandef performed_ch;Chandef receipt_ch;chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	

	if /* Rcv	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/external.go:7:12 */
	:: receipt_ch.deq?state,num_msgs /* 	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/external.go:7:12 */->
	:: receipt_ch.sync?state /* 	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/external.go:7:12 */ -> 
		receipt_ch.rcving!false
	fi->
	

	if /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/external.go:7:22 */
	:: adopted_ch.enq?ok -> 
		assert(ok) /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/external.go:7:22 */
	:: adopted_ch.sync!false -> 
		adopted_ch.sending?ok->
		assert(ok) /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/external.go:7:22 */
	fi->
	stop_process: skip->
	child!0
}

 /* ================================================================================== */
 /* ================================================================================== */
 /* ================================================================================== */ 
proctype async_monitor(Chandef ch) {
do
:: true ->
if
:: ch.closed -> 
end: if
  :: ch.num_msgs > 0 -> // cannot send on closed channel
    end4: if
    :: ch.enq!false-> // cannot send on closed channel
    :: ch.closing!true -> // cannot close twice a channel
    :: ch.rcving?false->
    :: ch.sending!false -> // sending state of channel (closed)
    :: ch.deq!true,ch.num_msgs -> 
  		 ch.num_msgs = ch.num_msgs - 1
    fi->
  :: else ->    end5: if
    :: ch.enq!false-> // cannot send on closed channel
    :: ch.closing!true -> // cannot close twice a channel
    :: ch.rcving?false->
    :: ch.sending!false -> // sending state of channel (closed)
    :: ch.sync!true-> 
    fi->
  fi->
:: else ->
	if
	:: ch.num_msgs == ch.size ->
		end1: if
		  :: ch.deq!false,ch.num_msgs ->
		    ch.num_msgs = ch.num_msgs - 1
		  :: ch.closing!false -> // closing the channel
		    ch.closed = true
		   :: ch.rcving?false ->
 		    ch.sending!true->
		fi->
	:: ch.num_msgs == 0 -> 
end2:		if
		:: ch.enq!true -> // a message has been received
			ch.num_msgs = ch.num_msgs + 1
		:: ch.closing!false -> // closing the channel
			ch.closed = true
		:: ch.rcving?false ->
 		    ch.sending!true->
		fi->
		:: else -> 
		end3: if
		  :: ch.enq!true->
		     ch.num_msgs = ch.num_msgs + 1
		  :: ch.deq!false,ch.num_msgs->
		     ch.num_msgs = ch.num_msgs - 1
		  :: ch.closing!false -> // closing the channel
		     ch.closed = true
		  :: ch.rcving?false ->
 		    ch.sending!true->
		fi->
	fi->
fi->
od->
}

proctype sync_monitor(Chandef ch) {
do
:: true->
if
:: ch.closed ->
end: if
  :: ch.enq!false-> // cannot send on closed channel
  :: ch.closing!true -> // cannot close twice a channel
  :: ch.sending!false -> // sending state of channel (closed)
  :: ch.rcving?false->
  :: ch.sync!true-> // can always receive on a closed chan
  fi->
:: else -> 
end1: if
    :: ch.rcving?false ->
       ch.sending!true->
    :: ch.closing!false ->
      ch.closed = true
    fi->
fi->
od->
stop_process: skip
}

proctype receiver(chan c) {
c?0
}

